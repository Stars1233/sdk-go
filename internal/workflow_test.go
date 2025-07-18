package internal

import (
	"fmt"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/enums/v1"

	"go.temporal.io/sdk/converter"
)

func TestGetChildWorkflowOptions(t *testing.T) {
	opts := ChildWorkflowOptions{
		Namespace:                "foo",
		WorkflowID:               "bar",
		TaskQueue:                "baz",
		WorkflowExecutionTimeout: 1,
		WorkflowRunTimeout:       2,
		WorkflowTaskTimeout:      3,
		WaitForCancellation:      true,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		RetryPolicy:              newTestRetryPolicy(),
		CronSchedule:             "todo",
		Memo: map[string]interface{}{
			"foo": "bar",
		},
		SearchAttributes: map[string]interface{}{
			"foo": "bar",
		},
		ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
		VersioningIntent:  VersioningIntentDefault,
		StaticSummary:     "child workflow summary",
		StaticDetails:     "child workflow details",
		Priority:          newPriority(),
	}

	// Require test options to have non-zero value for each field. This ensures that we update tests (and the
	// GetChildWorkflowOptions implementation) when new fields are added to the ChildWorkflowOptions struct.
	assertNonZero(t, opts)
	// Check that the same opts set on context are also extracted from context
	assert.Equal(t, opts, GetChildWorkflowOptions(WithChildWorkflowOptions(newTestWorkflowContext(), opts)))
}

func TestGetActivityOptions(t *testing.T) {
	opts := ActivityOptions{
		TaskQueue:              "foo",
		ScheduleToCloseTimeout: time.Millisecond,
		ScheduleToStartTimeout: time.Second,
		StartToCloseTimeout:    time.Minute,
		HeartbeatTimeout:       time.Hour,
		WaitForCancellation:    true,
		ActivityID:             "bar",
		RetryPolicy:            newTestRetryPolicy(),
		DisableEagerExecution:  true,
		VersioningIntent:       VersioningIntentDefault,
		Summary:                "activity summary",
		Priority:               newPriority(),
	}

	assertNonZero(t, opts)
	assert.Equal(t, opts, GetActivityOptions(WithActivityOptions(newTestWorkflowContext(), opts)))
}

func TestGetLocalActivityOptions(t *testing.T) {
	opts := LocalActivityOptions{
		ScheduleToCloseTimeout: time.Minute,
		StartToCloseTimeout:    time.Hour,
		RetryPolicy:            newTestRetryPolicy(),
		Summary:                "local activity summary",
	}

	assertNonZero(t, opts)
	assert.Equal(t, opts, GetLocalActivityOptions(WithLocalActivityOptions(newTestWorkflowContext(), opts)))
}

func TestConvertRetryPolicy(t *testing.T) {
	someDuration := time.Minute
	pbRetryPolicy := commonpb.RetryPolicy{
		InitialInterval:        durationpb.New(someDuration),
		MaximumInterval:        durationpb.New(someDuration),
		BackoffCoefficient:     1,
		MaximumAttempts:        2,
		NonRetryableErrorTypes: []string{"some_error"},
	}

	assertNonZero(t, &pbRetryPolicy)
	// Check that converting from/to commonpb.RetryPolicy is transparent
	assert.Equal(t, &pbRetryPolicy, convertToPBRetryPolicy(convertFromPBRetryPolicy(&pbRetryPolicy)))
}

func newTestWorkflowContext() Context {
	_, ctx, err := newWorkflowContext(&workflowEnvironmentImpl{
		dataConverter: converter.GetDefaultDataConverter(),
		workflowInfo: &WorkflowInfo{
			Namespace:     "default",
			TaskQueueName: "default",
		},
	}, nil)
	if err != nil {
		panic(err)
	}
	return ctx
}

func newTestRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		InitialInterval:        1,
		BackoffCoefficient:     2,
		MaximumInterval:        3,
		MaximumAttempts:        4,
		NonRetryableErrorTypes: []string{"my_error"},
	}
}

func newPriority() Priority {
	return Priority{
		PriorityKey: 1,
	}
}

// assertNonZero checks that every top level value, struct field, and item in a slice is a non-zero value.
func assertNonZero(t *testing.T, i interface{}) {
	_assertNonZero(t, i, reflect.ValueOf(i).Type().Name())
}

// Matches when a method should be private (by name)
// Sure, this only works for latin characters. It's fine enough for the test
var isPrivate = regexp.MustCompile("^[a-z]")

func _assertNonZero(t *testing.T, i interface{}, prefix string) {
	v := reflect.ValueOf(i)
	vt := v.Type()
	switch v.Kind() {
	case reflect.Struct:
		switch vx := v.Interface().(type) {
		case timestamppb.Timestamp:
			if vx.Nanos == 0 && vx.Seconds == 0 {
				t.Errorf("%s: value of type %T must be non-zero", prefix, i)
			}
			return
		case durationpb.Duration:
			if vx.Nanos == 0 && vx.Seconds == 0 {
				t.Errorf("%s: value of type %T must be non-zero", prefix, i)
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if isPrivate.MatchString(vt.Field(i).Name) {
				continue
			}
			_assertNonZero(t, v.Field(i).Interface(), fmt.Sprintf("%s.%s", prefix, v.Type().Field(i).Name))
		}
	case reflect.Slice:
		if v.Len() == 0 {
			t.Errorf("%s: value of type %T must have non-zero length", prefix, i)
		}
		for i := 0; i < v.Len(); i++ {
			_assertNonZero(t, v.Index(i).Interface(), fmt.Sprintf("%s[%d]", prefix, i))
		}
	case reflect.Ptr:
		if v.IsNil() {
			t.Errorf("%s: value of type %T must be non-nil", prefix, i)
		} else {
			_assertNonZero(t, reflect.Indirect(v).Interface(), prefix)
		}
	default:
		if v.IsZero() {
			t.Errorf("%s: value of type %T must be non-zero", prefix, i)
		}
	}
}

func TestDeterministicKeys(t *testing.T) {
	t.Parallel()

	var tests = []struct {
		unsorted map[int]int
		sorted   []int
	}{
		{
			map[int]int{1: 1, 2: 2, 3: 3},
			[]int{1, 2, 3},
		},
		{
			map[int]int{},
			[]int{},
		},
		{
			map[int]int{1: 1, 5: 5, 3: 3},
			[]int{1, 3, 5},
		},
		{
			map[int]int{3: 3, 2: 2, 1: 1},
			[]int{1, 2, 3},
		},
	}

	for _, tt := range tests {
		testname := fmt.Sprintf("%d,%d", tt.unsorted, tt.sorted)
		t.Run(testname, func(t *testing.T) {
			assert.Equal(t, tt.sorted, DeterministicKeys(tt.unsorted))
		})
	}
}

func TestDeterministicKeysFunc(t *testing.T) {
	t.Parallel()

	type keyStruct struct {
		i int
	}

	var tests = []struct {
		unsorted map[keyStruct]int
		sorted   []keyStruct
	}{
		{
			map[keyStruct]int{{1}: 1, {2}: 2, {3}: 3},
			[]keyStruct{{1}, {2}, {3}},
		},
		{
			map[keyStruct]int{},
			[]keyStruct{},
		},
		{
			map[keyStruct]int{{1}: 1, {5}: 5, {3}: 3},
			[]keyStruct{{1}, {3}, {5}},
		},
		{
			map[keyStruct]int{{3}: 3, {2}: 2, {1}: 1},
			[]keyStruct{{1}, {2}, {3}},
		},
	}

	for _, tt := range tests {
		testname := fmt.Sprintf("%d,%d", tt.unsorted, tt.sorted)
		t.Run(testname, func(t *testing.T) {
			assert.Equal(t, tt.sorted, DeterministicKeysFunc(tt.unsorted, func(a, b keyStruct) int {
				return a.i - b.i
			}))
		})
	}
}
