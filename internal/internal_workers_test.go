package internal

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/api/workflowservicemock/v1"

	"go.temporal.io/sdk/converter"
	ilog "go.temporal.io/sdk/internal/log"
)

// ActivityTaskHandler never returns response
type noResponseActivityTaskHandler struct {
	isExecuteCalled chan struct{}
}

func newNoResponseActivityTaskHandler() *noResponseActivityTaskHandler {
	return &noResponseActivityTaskHandler{isExecuteCalled: make(chan struct{})}
}

func (ath noResponseActivityTaskHandler) Execute(string, *workflowservice.PollActivityTaskQueueResponse) (interface{}, error) {
	close(ath.isExecuteCalled)
	c := make(chan struct{})
	<-c
	return nil, nil
}

func (ath noResponseActivityTaskHandler) BlockedOnExecuteCalled() error {
	<-ath.isExecuteCalled
	return nil
}

type (
	WorkersTestSuite struct {
		suite.Suite
		mockCtrl      *gomock.Controller
		service       *workflowservicemock.MockWorkflowServiceClient
		dataConverter converter.DataConverter
	}
)

// Test suite.
func (s *WorkersTestSuite) SetupTest() {
	s.mockCtrl = gomock.NewController(s.T())
	s.service = workflowservicemock.NewMockWorkflowServiceClient(s.mockCtrl)
	s.service.EXPECT().GetSystemInfo(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.GetSystemInfoResponse{}, nil).AnyTimes()
	s.dataConverter = converter.GetDefaultDataConverter()
}

func (s *WorkersTestSuite) TearDownTest() {
	s.mockCtrl.Finish() // assert mock’s expectations
}

func TestWorkersTestSuite(t *testing.T) {
	suite.Run(t, new(WorkersTestSuite))
}

func (s *WorkersTestSuite) TestWorkflowWorker() {
	s.service.EXPECT().DescribeNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	s.service.EXPECT().PollWorkflowTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.PollWorkflowTaskQueueResponse{}, nil).AnyTimes()
	s.service.EXPECT().RespondWorkflowTaskCompleted(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	s.service.EXPECT().ShutdownWorker(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.ShutdownWorkerResponse{}, nil).Times(1)

	ctx, cancel := context.WithCancelCause(context.Background())
	executionParameters := workerExecutionParameters{
		Namespace: DefaultNamespace,
		TaskQueue: "testTaskQueue",
		WorkflowTaskPollerBehavior: NewPollerBehaviorSimpleMaximum(
			PollerBehaviorSimpleMaximumOptions{
				MaximumNumberOfPollers: 5,
			},
		),
		Logger:                  ilog.NewDefaultLogger(),
		BackgroundContext:       ctx,
		BackgroundContextCancel: cancel,
	}
	overrides := &workerOverrides{workflowTaskHandler: newSampleWorkflowTaskHandler()}
	client := &WorkflowClient{workflowService: s.service}
	workflowWorker := newWorkflowWorkerInternal(client, executionParameters, nil, overrides, newRegistry())
	_ = workflowWorker.Start()
	workflowWorker.Stop()

	s.NoError(ctx.Err())

}

type CountingSlotSupplier struct {
	reserves, releases, uses atomic.Int32
}

func (c *CountingSlotSupplier) ReserveSlot(ctx context.Context, _ SlotReservationInfo) (*SlotPermit, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.reserves.Add(1)
	return &SlotPermit{}, nil
}

func (c *CountingSlotSupplier) TryReserveSlot(SlotReservationInfo) *SlotPermit {
	c.reserves.Add(1)
	return &SlotPermit{}
}

func (c *CountingSlotSupplier) MarkSlotUsed(SlotMarkUsedInfo) {
	c.uses.Add(1)
}

func (c *CountingSlotSupplier) ReleaseSlot(SlotReleaseInfo) {
	c.releases.Add(1)
}

func (c *CountingSlotSupplier) MaxSlots() int {
	return 5
}

func (s *WorkersTestSuite) TestWorkflowWorkerSlotSupplier() {
	// Run this a bunch of times since releases/reserves are sensitive to shutdown conditions
	// and we want to make sure they always line up
	for i := 0; i < 50; i++ {
		s.SetupTest()
		taskQueue := "testTaskQueue"
		testEvents := []*historypb.HistoryEvent{
			createTestEventWorkflowExecutionStarted(1, &historypb.WorkflowExecutionStartedEventAttributes{
				TaskQueue: &taskqueuepb.TaskQueue{Name: taskQueue},
			}),
			createTestEventWorkflowTaskScheduled(2, &historypb.WorkflowTaskScheduledEventAttributes{}),
			createTestEventWorkflowTaskStarted(3),
		}
		workflowType := "testReplayWorkflow"
		workflowID := "testID"
		runID := "testRunID"

		task := &workflowservice.PollWorkflowTaskQueueResponse{
			TaskToken:              []byte("test-token"),
			WorkflowExecution:      &commonpb.WorkflowExecution{WorkflowId: workflowID, RunId: runID},
			WorkflowType:           &commonpb.WorkflowType{Name: workflowType},
			History:                &historypb.History{Events: testEvents},
			PreviousStartedEventId: 0,
		}

		unblockPollCh := make(chan struct{})
		pollRespondedCh := make(chan struct{})
		s.service.EXPECT().DescribeNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
		s.service.EXPECT().PollWorkflowTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).
			Do(func(ctx, in interface{}, opts ...interface{}) {
				<-unblockPollCh
			}).
			Return(task, nil).AnyTimes()
		s.service.EXPECT().RespondWorkflowTaskCompleted(gomock.Any(), gomock.Any(), gomock.Any()).
			Do(func(ctx, in interface{}, opts ...interface{}) {
				pollRespondedCh <- struct{}{}
			}).
			Return(nil, nil).AnyTimes()
		s.service.EXPECT().ShutdownWorker(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.ShutdownWorkerResponse{}, nil).Times(1)

		ctx, cancel := context.WithCancelCause(context.Background())
		wfCss := &CountingSlotSupplier{}
		laCss := &CountingSlotSupplier{}
		tuner, err := NewCompositeTuner(CompositeTunerOptions{
			WorkflowSlotSupplier:      wfCss,
			ActivitySlotSupplier:      nil,
			LocalActivitySlotSupplier: laCss})
		s.NoError(err)
		executionParameters := workerExecutionParameters{
			Namespace: DefaultNamespace,
			TaskQueue: taskQueue,
			WorkflowTaskPollerBehavior: NewPollerBehaviorSimpleMaximum(
				PollerBehaviorSimpleMaximumOptions{
					MaximumNumberOfPollers: 5,
				},
			),
			Logger:                  ilog.NewDefaultLogger(),
			BackgroundContext:       ctx,
			BackgroundContextCancel: cancel,
			Tuner:                   tuner,
			WorkerStopTimeout:       time.Second,
		}
		overrides := &workerOverrides{workflowTaskHandler: newSampleWorkflowTaskHandler()}
		client := &WorkflowClient{workflowService: s.service}
		workflowWorker := newWorkflowWorkerInternal(client, executionParameters, nil, overrides, newRegistry())
		_ = workflowWorker.Start()
		unblockPollCh <- struct{}{}
		<-pollRespondedCh
		workflowWorker.Stop()

		s.Equal(int32(1), wfCss.uses.Load())
		// The number of reserves and releases should be equal
		s.Equal(wfCss.reserves.Load(), wfCss.releases.Load())
		s.NoError(ctx.Err())
	}
}

func (s *WorkersTestSuite) TestActivityWorkerSlotSupplier() {
	// Run this a bunch of times since releases/reserves are sensitive to shutdown conditions
	// and we want to make sure they always line up
	for i := 0; i < 50; i++ {
		s.SetupTest()

		task := &workflowservice.PollActivityTaskQueueResponse{
			TaskToken:         []byte("test-token"),
			WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: workflowID, RunId: runID},
			WorkflowType:      &commonpb.WorkflowType{Name: workflowType},
			ActivityId:        "activityID",
			ActivityType:      &commonpb.ActivityType{Name: "activityType"},
		}

		unblockPollCh := make(chan struct{})
		pollRespondedCh := make(chan struct{})
		s.service.EXPECT().DescribeNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
		s.service.EXPECT().PollActivityTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).
			Do(func(ctx, in interface{}, opts ...interface{}) {
				<-unblockPollCh
			}).
			Return(task, nil).AnyTimes()
		s.service.EXPECT().RespondActivityTaskCompleted(gomock.Any(), gomock.Any(), gomock.Any()).
			Do(func(ctx, in interface{}, opts ...interface{}) {
				pollRespondedCh <- struct{}{}
			}).
			Return(nil, nil).AnyTimes()

		actCss := &CountingSlotSupplier{}
		tuner, err := NewCompositeTuner(CompositeTunerOptions{
			WorkflowSlotSupplier:      nil,
			ActivitySlotSupplier:      actCss,
			LocalActivitySlotSupplier: nil})
		s.NoError(err)
		executionParameters := workerExecutionParameters{
			Namespace: DefaultNamespace,
			TaskQueue: "testTaskQueue",
			ActivityTaskPollerBehavior: NewPollerBehaviorSimpleMaximum(
				PollerBehaviorSimpleMaximumOptions{
					MaximumNumberOfPollers: 5,
				},
			),
			Logger:            ilog.NewDefaultLogger(),
			Tuner:             tuner,
			WorkerStopTimeout: time.Second,
		}
		overrides := &workerOverrides{activityTaskHandler: newSampleActivityTaskHandler()}
		a := &greeterActivity{}
		registry := newRegistry()
		registry.addActivityWithLock(a.ActivityType().Name, a)
		client := WorkflowClient{workflowService: s.service}
		activityWorker := newActivityWorker(&client, executionParameters, overrides, registry, nil)
		_ = activityWorker.Start()
		unblockPollCh <- struct{}{}
		<-pollRespondedCh
		activityWorker.Stop()

		s.Equal(int32(1), actCss.uses.Load())
		// The number of reserves and releases should be equal
		s.Equal(actCss.reserves.Load(), actCss.releases.Load())
	}
}

type SometimesFailSlotSupplier struct {
	failEveryN  int
	currentSlot atomic.Int32
}

func (s *SometimesFailSlotSupplier) ReserveSlot(_ context.Context, info SlotReservationInfo) (*SlotPermit, error) {
	if int(s.currentSlot.Load())%s.failEveryN == 0 {
		s.currentSlot.Add(1)
		return nil, errors.New("ahhhhh fail")
	}
	return s.TryReserveSlot(info), nil
}
func (s *SometimesFailSlotSupplier) TryReserveSlot(SlotReservationInfo) *SlotPermit {
	s.currentSlot.Add(1)
	return &SlotPermit{}
}
func (s *SometimesFailSlotSupplier) MarkSlotUsed(SlotMarkUsedInfo) {}
func (s *SometimesFailSlotSupplier) ReleaseSlot(SlotReleaseInfo)   {}
func (s *SometimesFailSlotSupplier) MaxSlots() int                 { return 0 }

func (s *WorkersTestSuite) TestErrorProneSlotSupplier() {
	s.SetupTest()

	task := &workflowservice.PollActivityTaskQueueResponse{
		TaskToken:         []byte("test-token"),
		WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: workflowID, RunId: runID},
		WorkflowType:      &commonpb.WorkflowType{Name: workflowType},
		ActivityId:        "activityID",
		ActivityType:      &commonpb.ActivityType{Name: "activityType"},
	}

	unblockPollCh := make(chan struct{})
	pollRespondedCh := make(chan struct{})
	s.service.EXPECT().DescribeNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	s.service.EXPECT().PollActivityTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(ctx, in interface{}, opts ...interface{}) {
			<-unblockPollCh
		}).
		Return(task, nil).AnyTimes()
	s.service.EXPECT().RespondActivityTaskCompleted(gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(ctx, in interface{}, opts ...interface{}) {
			pollRespondedCh <- struct{}{}
		}).
		Return(nil, nil).AnyTimes()

	actCss := &SometimesFailSlotSupplier{failEveryN: 5}
	tuner, err := NewCompositeTuner(CompositeTunerOptions{
		WorkflowSlotSupplier:      nil,
		ActivitySlotSupplier:      actCss,
		LocalActivitySlotSupplier: nil})
	s.NoError(err)
	executionParameters := workerExecutionParameters{
		Namespace: DefaultNamespace,
		TaskQueue: "testTaskQueue",
		ActivityTaskPollerBehavior: NewPollerBehaviorSimpleMaximum(
			PollerBehaviorSimpleMaximumOptions{
				MaximumNumberOfPollers: 5,
			},
		),
		Logger:            ilog.NewDefaultLogger(),
		Tuner:             tuner,
		WorkerStopTimeout: time.Second,
	}
	overrides := &workerOverrides{activityTaskHandler: newSampleActivityTaskHandler()}
	a := &greeterActivity{}
	registry := newRegistry()
	registry.addActivityWithLock(a.ActivityType().Name, a)
	client := WorkflowClient{workflowService: s.service}
	activityWorker := newActivityWorker(&client, executionParameters, overrides, registry, nil)
	_ = activityWorker.Start()
	for i := 0; i < 25; i++ {
		unblockPollCh <- struct{}{}
		<-pollRespondedCh
	}
	activityWorker.Stop()
}

func (s *WorkersTestSuite) TestActivityWorker() {
	s.service.EXPECT().DescribeNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	s.service.EXPECT().PollActivityTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.PollActivityTaskQueueResponse{}, nil).AnyTimes()
	s.service.EXPECT().RespondActivityTaskCompleted(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.RespondActivityTaskCompletedResponse{}, nil).AnyTimes()

	executionParameters := workerExecutionParameters{
		Namespace: DefaultNamespace,
		TaskQueue: "testTaskQueue",
		ActivityTaskPollerBehavior: NewPollerBehaviorSimpleMaximum(
			PollerBehaviorSimpleMaximumOptions{
				MaximumNumberOfPollers: 5,
			},
		),
		Logger: ilog.NewDefaultLogger(),
	}
	overrides := &workerOverrides{activityTaskHandler: newSampleActivityTaskHandler()}
	a := &greeterActivity{}
	registry := newRegistry()
	registry.addActivityWithLock(a.ActivityType().Name, a)
	client := WorkflowClient{workflowService: s.service}
	activityWorker := newActivityWorker(&client, executionParameters, overrides, registry, nil)
	_ = activityWorker.Start()
	activityWorker.Stop()
}

func (s *WorkersTestSuite) TestActivityWorkerStop() {
	now := time.Now()

	pats := &workflowservice.PollActivityTaskQueueResponse{
		Attempt:   1,
		TaskToken: []byte("token"),
		WorkflowExecution: &commonpb.WorkflowExecution{
			WorkflowId: "wID",
			RunId:      "rID",
		},
		ActivityType:           &commonpb.ActivityType{Name: "test"},
		ActivityId:             uuid.NewString(),
		ScheduledTime:          timestamppb.New(now),
		ScheduleToCloseTimeout: durationpb.New(1 * time.Second),
		StartedTime:            timestamppb.New(now),
		StartToCloseTimeout:    durationpb.New(1 * time.Second),
		WorkflowType: &commonpb.WorkflowType{
			Name: "wType",
		},
		WorkflowNamespace: "namespace",
	}

	s.service.EXPECT().DescribeNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	s.service.EXPECT().PollActivityTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(pats, nil).AnyTimes()
	s.service.EXPECT().RespondActivityTaskCompleted(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.RespondActivityTaskCompletedResponse{}, nil).AnyTimes()

	stopC := make(chan struct{})
	ctx, cancel := context.WithCancelCause(context.Background())
	tuner, err := NewFixedSizeTuner(FixedSizeTunerOptions{
		NumWorkflowSlots:      defaultMaxConcurrentTaskExecutionSize,
		NumActivitySlots:      2,
		NumLocalActivitySlots: defaultMaxConcurrentLocalActivityExecutionSize})
	s.NoError(err)
	executionParameters := workerExecutionParameters{
		Namespace: DefaultNamespace,
		TaskQueue: "testTaskQueue",
		ActivityTaskPollerBehavior: NewPollerBehaviorSimpleMaximum(
			PollerBehaviorSimpleMaximumOptions{
				MaximumNumberOfPollers: 5,
			},
		),
		Tuner:                   tuner,
		Logger:                  ilog.NewDefaultLogger(),
		BackgroundContext:       ctx,
		BackgroundContextCancel: cancel,
		WorkerStopTimeout:       time.Second * 2,
		WorkerStopChannel:       stopC,
	}
	activityTaskHandler := newNoResponseActivityTaskHandler()
	overrides := &workerOverrides{activityTaskHandler: activityTaskHandler}
	a := &greeterActivity{}
	registry := newRegistry()
	registry.addActivityWithLock(a.ActivityType().Name, a)
	client := WorkflowClient{workflowService: s.service}
	worker := newActivityWorker(&client, executionParameters, overrides, registry, nil)
	_ = worker.Start()
	_ = activityTaskHandler.BlockedOnExecuteCalled()
	go worker.Stop()

	<-worker.worker.stopCh
	err = ctx.Err()
	s.NoError(err)

	<-ctx.Done()
	err = ctx.Err()
	s.Error(err)
	s.ErrorIs(context.Cause(ctx), ErrWorkerShutdown)
}

func (s *WorkersTestSuite) TestPollWorkflowTaskQueue_InternalServiceError() {
	s.service.EXPECT().DescribeNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	s.service.EXPECT().PollWorkflowTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.PollWorkflowTaskQueueResponse{}, serviceerror.NewInternal("")).AnyTimes()
	s.service.EXPECT().ShutdownWorker(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.ShutdownWorkerResponse{}, nil).Times(1)

	executionParameters := workerExecutionParameters{
		Namespace: DefaultNamespace,
		TaskQueue: "testWorkflowTaskQueue",
		WorkflowTaskPollerBehavior: NewPollerBehaviorSimpleMaximum(
			PollerBehaviorSimpleMaximumOptions{
				MaximumNumberOfPollers: 5,
			},
		),
		Logger: ilog.NewNopLogger(),
	}
	overrides := &workerOverrides{workflowTaskHandler: newSampleWorkflowTaskHandler()}
	client := &WorkflowClient{workflowService: s.service}
	workflowWorker := newWorkflowWorkerInternal(client, executionParameters, nil, overrides, newRegistry())
	_ = workflowWorker.Start()
	workflowWorker.Stop()
}

func (s *WorkersTestSuite) TestLongRunningWorkflowTask() {
	localActivityCalledCount := 0
	localActivitySleep := func(duration time.Duration) error {
		time.Sleep(duration)
		localActivityCalledCount++
		return nil
	}

	doneCh := make(chan struct{})

	isWorkflowCompleted := false
	longWorkflowTaskWorkflowFn := func(ctx Context, input []byte) error {
		lao := LocalActivityOptions{
			ScheduleToCloseTimeout: time.Second * 2,
		}
		ctx = WithLocalActivityOptions(ctx, lao)
		err := ExecuteLocalActivity(ctx, localActivitySleep, time.Second).Get(ctx, nil)
		if err != nil {
			return err
		}

		err = ExecuteLocalActivity(ctx, localActivitySleep, time.Second).Get(ctx, nil)
		isWorkflowCompleted = true
		return err
	}

	taskQueue := "long-running-workflow-task-tq"
	testEvents := []*historypb.HistoryEvent{
		{
			EventId:   1,
			EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED,
			Attributes: &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{
				TaskQueue:                &taskqueuepb.TaskQueue{Name: taskQueue},
				WorkflowExecutionTimeout: durationpb.New(10 * time.Second),
				WorkflowRunTimeout:       durationpb.New(10 * time.Second),
				WorkflowTaskTimeout:      durationpb.New(2 * time.Second),
				WorkflowType:             &commonpb.WorkflowType{Name: "long-running-workflow-task-workflow-type"},
			}},
		},
		createTestEventWorkflowTaskScheduled(2, &historypb.WorkflowTaskScheduledEventAttributes{TaskQueue: &taskqueuepb.TaskQueue{Name: taskQueue}}),
		createTestEventWorkflowTaskStarted(3),
		createTestEventWorkflowTaskCompleted(4, &historypb.WorkflowTaskCompletedEventAttributes{ScheduledEventId: 2}),
		{
			EventId:   5,
			EventType: enumspb.EVENT_TYPE_MARKER_RECORDED,
			Attributes: &historypb.HistoryEvent_MarkerRecordedEventAttributes{MarkerRecordedEventAttributes: &historypb.MarkerRecordedEventAttributes{
				MarkerName:                   localActivityMarkerName,
				Details:                      s.createLocalActivityMarkerDataForTest("0"),
				WorkflowTaskCompletedEventId: 4,
			}},
		},
		createTestEventWorkflowTaskScheduled(6, &historypb.WorkflowTaskScheduledEventAttributes{TaskQueue: &taskqueuepb.TaskQueue{Name: taskQueue}}),
		createTestEventWorkflowTaskStarted(7),
		createTestEventWorkflowTaskCompleted(8, &historypb.WorkflowTaskCompletedEventAttributes{ScheduledEventId: 2}),
		{
			EventId:   9,
			EventType: enumspb.EVENT_TYPE_MARKER_RECORDED,
			Attributes: &historypb.HistoryEvent_MarkerRecordedEventAttributes{MarkerRecordedEventAttributes: &historypb.MarkerRecordedEventAttributes{
				MarkerName:                   localActivityMarkerName,
				Details:                      s.createLocalActivityMarkerDataForTest("1"),
				WorkflowTaskCompletedEventId: 8,
			}},
		},
		createTestEventWorkflowTaskScheduled(10, &historypb.WorkflowTaskScheduledEventAttributes{TaskQueue: &taskqueuepb.TaskQueue{Name: taskQueue}}),
		createTestEventWorkflowTaskStarted(11),
	}

	s.service.EXPECT().DescribeNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	task := &workflowservice.PollWorkflowTaskQueueResponse{
		TaskToken: []byte("test-token"),
		WorkflowExecution: &commonpb.WorkflowExecution{
			WorkflowId: "long-running-workflow-task-workflow-id",
			RunId:      "long-running-workflow-task-workflow-run-id",
		},
		WorkflowType: &commonpb.WorkflowType{
			Name: "long-running-workflow-task-workflow-type",
		},
		PreviousStartedEventId: 0,
		StartedEventId:         3,
		History:                &historypb.History{Events: testEvents[0:3]},
		NextPageToken:          nil,
	}
	s.service.EXPECT().PollWorkflowTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.PollWorkflowTaskQueueResponse{}, serviceerror.NewInvalidArgument("")).Times(1)
	s.service.EXPECT().PollWorkflowTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(task, nil).Times(1)
	s.service.EXPECT().PollWorkflowTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.PollWorkflowTaskQueueResponse{}, serviceerror.NewInternal("")).AnyTimes()
	s.service.EXPECT().PollActivityTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.PollActivityTaskQueueResponse{}, nil).AnyTimes()

	respondCounter := 0
	s.service.EXPECT().RespondWorkflowTaskCompleted(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, request *workflowservice.RespondWorkflowTaskCompletedRequest, opts ...grpc.CallOption,
	) (success *workflowservice.RespondWorkflowTaskCompletedResponse, err error) {
		respondCounter++
		switch respondCounter {
		case 1:
			s.Equal(1, len(request.Commands))
			s.Equal(enumspb.COMMAND_TYPE_RECORD_MARKER, request.Commands[0].GetCommandType())
			task.PreviousStartedEventId = 3
			task.StartedEventId = 7
			task.History.Events = testEvents[3:7]
			return &workflowservice.RespondWorkflowTaskCompletedResponse{WorkflowTask: task}, nil
		case 2:
			s.Equal(2, len(request.Commands))
			s.Equal(enumspb.COMMAND_TYPE_RECORD_MARKER, request.Commands[0].GetCommandType())
			s.Equal(enumspb.COMMAND_TYPE_COMPLETE_WORKFLOW_EXECUTION, request.Commands[1].GetCommandType())
			task.PreviousStartedEventId = 7
			task.StartedEventId = 11
			task.History.Events = testEvents[7:11]
			close(doneCh)
			return nil, nil
		default:
			panic("unexpected RespondWorkflowTaskCompleted")
		}
	}).Times(2)

	s.service.EXPECT().ShutdownWorker(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&workflowservice.ShutdownWorkerResponse{}, nil).Times(1)

	clientOptions := ClientOptions{
		Identity: "test-worker-identity",
	}

	client := NewServiceClient(s.service, nil, clientOptions)
	worker := NewAggregatedWorker(client, taskQueue, WorkerOptions{})
	worker.RegisterWorkflowWithOptions(
		longWorkflowTaskWorkflowFn,
		RegisterWorkflowOptions{Name: "long-running-workflow-task-workflow-type"},
	)
	worker.RegisterActivity(localActivitySleep)

	_ = worker.Start()
	// wait for test to complete
	select {
	case <-doneCh:
		break
	case <-time.After(time.Second * 4):
	}
	worker.Stop()

	s.True(isWorkflowCompleted)
	s.Equal(2, localActivityCalledCount)
}

func (s *WorkersTestSuite) TestMultipleLocalActivities() {
	localActivityCalledCount := 0
	localActivitySleep := func(duration time.Duration) error {
		time.Sleep(duration)
		localActivityCalledCount++
		return nil
	}

	doneCh := make(chan struct{})

	isWorkflowCompleted := false
	longWorkflowTaskWorkflowFn := func(ctx Context, input []byte) error {
		lao := LocalActivityOptions{
			ScheduleToCloseTimeout: time.Second * 2,
		}
		ctx = WithLocalActivityOptions(ctx, lao)
		err := ExecuteLocalActivity(ctx, localActivitySleep, time.Second).Get(ctx, nil)
		if err != nil {
			return err
		}

		err = ExecuteLocalActivity(ctx, localActivitySleep, time.Second).Get(ctx, nil)
		isWorkflowCompleted = true
		return err
	}

	taskQueue := "multiple-local-activities-tq"
	testEvents := []*historypb.HistoryEvent{
		{
			EventId:   1,
			EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED,
			Attributes: &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{
				TaskQueue:                &taskqueuepb.TaskQueue{Name: taskQueue},
				WorkflowExecutionTimeout: durationpb.New(10 * time.Second),
				WorkflowRunTimeout:       durationpb.New(10 * time.Second),
				WorkflowTaskTimeout:      durationpb.New(3 * time.Second),
				WorkflowType:             &commonpb.WorkflowType{Name: "multiple-local-activities-workflow-type"},
			}},
		},
		createTestEventWorkflowTaskScheduled(2, &historypb.WorkflowTaskScheduledEventAttributes{TaskQueue: &taskqueuepb.TaskQueue{Name: taskQueue}}),
		createTestEventWorkflowTaskStarted(3),
		createTestEventWorkflowTaskCompleted(4, &historypb.WorkflowTaskCompletedEventAttributes{ScheduledEventId: 2}),
		{
			EventId:   5,
			EventType: enumspb.EVENT_TYPE_MARKER_RECORDED,
			Attributes: &historypb.HistoryEvent_MarkerRecordedEventAttributes{MarkerRecordedEventAttributes: &historypb.MarkerRecordedEventAttributes{
				MarkerName:                   localActivityMarkerName,
				Details:                      s.createLocalActivityMarkerDataForTest("0"),
				WorkflowTaskCompletedEventId: 4,
			}},
		},
		createTestEventWorkflowTaskScheduled(6, &historypb.WorkflowTaskScheduledEventAttributes{TaskQueue: &taskqueuepb.TaskQueue{Name: taskQueue}}),
		createTestEventWorkflowTaskStarted(7),
		createTestEventWorkflowTaskCompleted(8, &historypb.WorkflowTaskCompletedEventAttributes{ScheduledEventId: 2}),
		{
			EventId:   9,
			EventType: enumspb.EVENT_TYPE_MARKER_RECORDED,
			Attributes: &historypb.HistoryEvent_MarkerRecordedEventAttributes{MarkerRecordedEventAttributes: &historypb.MarkerRecordedEventAttributes{
				MarkerName:                   localActivityMarkerName,
				Details:                      s.createLocalActivityMarkerDataForTest("1"),
				WorkflowTaskCompletedEventId: 8,
			}},
		},
		createTestEventWorkflowTaskScheduled(10, &historypb.WorkflowTaskScheduledEventAttributes{TaskQueue: &taskqueuepb.TaskQueue{Name: taskQueue}}),
		createTestEventWorkflowTaskStarted(11),
	}

	s.service.EXPECT().DescribeNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	task := &workflowservice.PollWorkflowTaskQueueResponse{
		TaskToken: []byte("test-token"),
		WorkflowExecution: &commonpb.WorkflowExecution{
			WorkflowId: "multiple-local-activities-workflow-id",
			RunId:      "multiple-local-activities-workflow-run-id",
		},
		WorkflowType: &commonpb.WorkflowType{
			Name: "multiple-local-activities-workflow-type",
		},
		PreviousStartedEventId: 0,
		StartedEventId:         3,
		History:                &historypb.History{Events: testEvents[0:3]},
		NextPageToken:          nil,
	}
	s.service.EXPECT().PollWorkflowTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(task, nil).Times(1)
	s.service.EXPECT().PollWorkflowTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.PollWorkflowTaskQueueResponse{}, serviceerror.NewInternal("")).AnyTimes()
	s.service.EXPECT().PollActivityTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).Return(&workflowservice.PollActivityTaskQueueResponse{}, nil).AnyTimes()

	respondCounter := 0
	s.service.EXPECT().RespondWorkflowTaskCompleted(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, request *workflowservice.RespondWorkflowTaskCompletedRequest, opts ...grpc.CallOption,
	) (success *workflowservice.RespondWorkflowTaskCompletedResponse, err error) {
		respondCounter++
		switch respondCounter {
		case 1:
			s.Equal(3, len(request.Commands))
			s.Equal(enumspb.COMMAND_TYPE_RECORD_MARKER, request.Commands[0].GetCommandType())
			task.PreviousStartedEventId = 3
			task.StartedEventId = 7
			task.History.Events = testEvents[3:11]
			close(doneCh)
			return nil, nil
		default:
			panic("unexpected RespondWorkflowTaskCompleted")
		}
	}).Times(1)

	s.service.EXPECT().ShutdownWorker(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&workflowservice.ShutdownWorkerResponse{}, nil).Times(1)

	clientOptions := ClientOptions{
		Identity: "test-worker-identity",
	}

	client := NewServiceClient(s.service, nil, clientOptions)
	worker := NewAggregatedWorker(client, taskQueue, WorkerOptions{})
	worker.RegisterWorkflowWithOptions(
		longWorkflowTaskWorkflowFn,
		RegisterWorkflowOptions{Name: "multiple-local-activities-workflow-type"},
	)
	worker.RegisterActivity(localActivitySleep)

	_ = worker.Start()
	// wait for test to complete
	select {
	case <-doneCh:
		break
	case <-time.After(time.Second * 5):
	}
	worker.Stop()

	s.True(isWorkflowCompleted)
	s.Equal(2, localActivityCalledCount)
}

func (s *WorkersTestSuite) TestWorkerMultipleStop() {
	s.service.EXPECT().DescribeNamespace(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	s.service.EXPECT().PollWorkflowTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&workflowservice.PollWorkflowTaskQueueResponse{}, nil).AnyTimes()
	s.service.EXPECT().PollActivityTaskQueue(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&workflowservice.PollActivityTaskQueueResponse{}, nil).AnyTimes()
	s.service.EXPECT().ShutdownWorker(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&workflowservice.ShutdownWorkerResponse{}, nil).Times(1)

	client := NewServiceClient(s.service, nil, ClientOptions{Identity: "multi-stop-identity"})
	worker := NewAggregatedWorker(client, "multi-stop-tq", WorkerOptions{})
	s.NoError(worker.Start())
	worker.Stop()
	// Verify stopping the worker removes it from the eager dispatcher
	s.Empty(client.eagerDispatcher.workersByTaskQueue["multi-stop-tq"])
	worker.Stop()
}

func (s *WorkersTestSuite) TestWorkerTaskQueueLimitDisableEager() {
	client := NewServiceClient(s.service, nil, ClientOptions{Identity: "task-queue-limit-disable-eager"})
	worker := NewAggregatedWorker(client, "task-queue-limit-disable-eager", WorkerOptions{
		TaskQueueActivitiesPerSecond: 1.0,
	})
	s.True(worker.activityWorker.executionParameters.eagerActivityExecutor.disabled)
}

func (s *WorkersTestSuite) createLocalActivityMarkerDataForTest(activityID string) map[string]*commonpb.Payloads {
	lamd := localActivityMarkerData{
		ActivityID: activityID,
		ReplayTime: time.Now(),
	}

	// encode marker data
	markerData, err := s.dataConverter.ToPayloads(lamd)

	s.NoError(err)
	return map[string]*commonpb.Payloads{
		localActivityMarkerDataName: markerData,
	}
}
