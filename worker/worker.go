// Package worker contains functions to manage lifecycle of a Temporal client side worker.
package worker

import (
	"context"

	"github.com/nexus-rpc/sdk-go/nexus"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/workflowservice/v1"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/internal"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/workflow"
)

var (
	// ErrWorkerShutdown is returned when the worker is shutdown.
	ErrWorkerShutdown = internal.ErrWorkerShutdown
)

type (
	// Worker hosts workflow and activity implementations.
	// Use worker.New(...) to create an instance.
	Worker interface {
		Registry

		// Start the worker in a non-blocking fashion.
		//
		// Note, this will only return errors on start. To catch errors during run,
		// use Run() instead or set Options.OnFatalError.
		Start() error

		// Run the worker in a blocking fashion. Stop the worker when interruptCh receives signal.
		// Pass worker.InterruptCh() to stop the worker with SIGINT or SIGTERM.
		// Pass nil to stop the worker with external Stop() call.
		// Pass any other `<-chan interface{}` and Run will wait for signal from that channel.
		// Returns error if the worker fails to start or there is a fatal error
		// during execution.
		//
		// Users are encouraged to use Start() instead of this call if they plan to
		// manually Stop(). Otherwise a race can occur if shutdown occurs before the
		// worker is started. This Run() call is only best if shutdown is initiated
		// via the interrupt channel.
		Run(interruptCh <-chan interface{}) error

		// Stop the worker.
		//
		// This may panic if called a second time.
		Stop()
	}

	// Registry exposes registration functions to consumers.
	Registry interface {
		WorkflowRegistry
		ActivityRegistry
		NexusServiceRegistry
	}

	// WorkflowRegistry exposes workflow registration functions to consumers.
	WorkflowRegistry interface {
		// RegisterWorkflow - registers a workflow function with the worker.
		// A workflow takes a [workflow.Context] and input and returns a (result, error) or just error.
		// Examples:
		//	func sampleWorkflow(ctx workflow.Context, input []byte) (result []byte, err error)
		//	func sampleWorkflow(ctx workflow.Context, arg1 int, arg2 string) (result []byte, err error)
		//	func sampleWorkflow(ctx workflow.Context) (result []byte, err error)
		//	func sampleWorkflow(ctx workflow.Context, arg1 int) (result string, err error)
		// Serialization of all primitive types, structures is supported ... except channels, functions, variadic, unsafe pointer.
		// For global registration consider workflow.Register
		// This method panics if workflowFunc doesn't comply with the expected format or tries to register the same workflow
		RegisterWorkflow(w interface{})

		// RegisterWorkflowWithOptions registers the workflow function with options.
		// The user can use options to provide an external name for the workflow or leave it empty if no
		// external name is required. This can be used as
		//  worker.RegisterWorkflowWithOptions(sampleWorkflow, RegisterWorkflowOptions{})
		//  worker.RegisterWorkflowWithOptions(sampleWorkflow, RegisterWorkflowOptions{Name: "foo"})
		// This method panics if workflowFunc doesn't comply with the expected format or tries to register the same workflow
		// type name twice. Use workflow.RegisterOptions.DisableAlreadyRegisteredCheck to allow multiple registrations.
		RegisterWorkflowWithOptions(w interface{}, options workflow.RegisterOptions)

		// RegisterDynamicWorkflow registers the dynamic workflow function with options.
		RegisterDynamicWorkflow(w interface{}, options workflow.DynamicRegisterOptions)
	}

	// ActivityRegistry exposes activity registration functions to consumers.
	ActivityRegistry interface {
		// RegisterActivity - register an activity function or a pointer to a structure with the worker.
		// An activity function takes a context and input and returns a (result, error) or just error.
		//
		// And activity struct is a structure with all its exported methods treated as activities. The default
		// name of each activity is the method name.
		//
		// Examples:
		//	func sampleActivity(ctx context.Context, input []byte) (result []byte, err error)
		//	func sampleActivity(ctx context.Context, arg1 int, arg2 string) (result *customerStruct, err error)
		//	func sampleActivity(ctx context.Context) (err error)
		//	func sampleActivity() (result string, err error)
		//	func sampleActivity(arg1 bool) (result int, err error)
		//	func sampleActivity(arg1 bool) (err error)
		//
		//  type Activities struct {
		//     // fields
		//  }
		//  func (a *Activities) SampleActivity1(ctx context.Context, arg1 int, arg2 string) (result *customerStruct, err error) {
		//    ...
		//  }
		//
		//  func (a *Activities) SampleActivity2(ctx context.Context, arg1 int, arg2 *customerStruct) (result string, err error) {
		//    ...
		//  }
		//
		// Serialization of all primitive types, structures is supported ... except channels, functions, variadic, unsafe pointer.
		// This method panics if activityFunc doesn't comply with the expected format or an activity with the same
		// type name is registered more than once.
		RegisterActivity(a interface{})

		// RegisterActivityWithOptions registers the activity function or struct pointer with options.
		// The user can use options to provide an external name for the activity or leave it empty if no
		// external name is required. This can be used as
		//  worker.RegisterActivityWithOptions(barActivity, RegisterActivityOptions{})
		//  worker.RegisterActivityWithOptions(barActivity, RegisterActivityOptions{Name: "barExternal"})
		// When registering the structure that implements activities the name is used as a prefix that is
		// prepended to the activity method name.
		//  worker.RegisterActivityWithOptions(&Activities{ ... }, RegisterActivityOptions{Name: "MyActivities_"})
		// To override each name of activities defined through a structure register the methods one by one:
		// activities := &Activities{ ... }
		// worker.RegisterActivityWithOptions(activities.SampleActivity1, RegisterActivityOptions{Name: "Sample1"})
		// worker.RegisterActivityWithOptions(activities.SampleActivity2, RegisterActivityOptions{Name: "Sample2"})
		// See RegisterActivity function for more info.
		// The other use of options is to disable duplicated activity registration check
		// which might be useful for integration tests.
		// worker.RegisterActivityWithOptions(barActivity, RegisterActivityOptions{DisableAlreadyRegisteredCheck: true})
		RegisterActivityWithOptions(a interface{}, options activity.RegisterOptions)

		// RegisterDynamicActivity registers the dynamic activity function with options.
		// Registering activities via a structure is not supported for dynamic activities.
		RegisterDynamicActivity(a interface{}, options activity.DynamicRegisterOptions)
	}

	// NexusServiceRegistry exposes Nexus Service registration functions.
	NexusServiceRegistry interface {
		// RegisterNexusService registers a service with a worker. Panics if a service with the same name has
		// already been registered on this worker or if the worker has already been started. A worker will only
		// poll for Nexus tasks if any services are registered on it.
		RegisterNexusService(*nexus.Service)
	}

	// WorkflowReplayer supports replaying a workflow from its event history.
	// Use for troubleshooting and backwards compatibility unit tests.
	// For example if a workflow failed in production then its history can be downloaded through UI or CLI
	// and replayed in a debugger as many times as necessary.
	// Use this class to create unit tests that check if workflow changes are backwards compatible.
	// It is important to maintain backwards compatibility through use of [workflow.GetVersion]
	// to ensure that new deployments are not going to break open workflows.
	WorkflowReplayer interface {
		// RegisterWorkflow registers workflow that is going to be replayed
		RegisterWorkflow(w interface{})

		// RegisterWorkflowWithOptions registers workflow that is going to be replayed with user provided name
		RegisterWorkflowWithOptions(w interface{}, options workflow.RegisterOptions)

		// RegisterDynamicWorkflow registers dynamic workflow that is going to be replayed
		RegisterDynamicWorkflow(w interface{}, options workflow.DynamicRegisterOptions)

		// ReplayWorkflowHistory executes a single workflow task for the given json history file.
		// Use for testing the backwards compatibility of code changes and troubleshooting workflows in a debugger.
		// The logger is an optional parameter. Defaults to the noop logger.
		//
		// History can be loaded from a reader with client.HistoryFromJSON.
		ReplayWorkflowHistory(logger log.Logger, history *historypb.History) error

		// ReplayWorkflowHistoryWithOptions executes a single workflow task for the given json history file.
		// Use for testing the backwards compatibility of code changes and troubleshooting workflows in a debugger.
		// The logger is an optional parameter. Defaults to the noop logger. Options allow aditional customization when
		// replaying this history.
		//
		// History can be loaded from a reader with client.HistoryFromJSON.
		ReplayWorkflowHistoryWithOptions(logger log.Logger, history *historypb.History, options ReplayWorkflowHistoryOptions) error

		// ReplayWorkflowHistoryFromJSONFile executes a single workflow task for the json history file downloaded from the cli.
		// To download the history file: temporal workflow show --workflow-id <workflow_id> --output json > <output_file>
		// See https://github.com/temporalio/temporal/blob/master/tools/cli/README.md for full documentation
		// Use for testing the backwards compatibility of code changes and troubleshooting workflows in a debugger.
		// The logger is an optional parameter. Defaults to the noop logger.
		ReplayWorkflowHistoryFromJSONFile(logger log.Logger, jsonfileName string) error

		// ReplayPartialWorkflowHistoryFromJSONFile executes a single workflow task for the json history file upto provided
		// lastEventID(inclusive), downloaded from the cli.
		// To download the history file: temporal workflow show --workflow-id <workflow_id> --output json > <output_file>
		// See https://github.com/temporalio/temporal/blob/master/tools/cli/README.md for full documentation
		// Use for testing the backwards compatibility of code changes and troubleshooting workflows in a debugger.
		// The logger is an optional parameter. Defaults to the noop logger.
		ReplayPartialWorkflowHistoryFromJSONFile(logger log.Logger, jsonfileName string, lastEventID int64) error

		// ReplayWorkflowExecution loads a workflow execution history from the Temporal service and executes a single workflow task for it.
		// Use for testing the backwards compatibility of code changes and troubleshooting workflows in a debugger.
		// The logger is the only optional parameter. Defaults to the noop logger. The Run ID and Workflow ID used during replay are derived
		// from execution.
		ReplayWorkflowExecution(ctx context.Context, service workflowservice.WorkflowServiceClient, logger log.Logger, namespace string, execution workflow.Execution) error
	}

	// DeploymentOptions provides configuration to enable Worker Versioning.
	//
	// NOTE: Experimental
	DeploymentOptions = internal.WorkerDeploymentOptions

	// WorkerDeploymentVersion represents a specific version of a worker in a deployment.
	//
	// NOTE: Experimental
	WorkerDeploymentVersion = internal.WorkerDeploymentVersion

	// Options is used to configure a worker instance.
	Options = internal.WorkerOptions

	// PollerBehavior is used to configure the behavior of the poller.
	//
	// NOTE: Experimental
	PollerBehavior = internal.PollerBehavior

	// PollerBehaviorAutoscalingOptions is the options for NewPollerBehaviorAutoscaling.
	//
	// NOTE: Experimental
	PollerBehaviorAutoscalingOptions = internal.PollerBehaviorAutoscalingOptions

	// PollerBehaviorSimpleMaximumOptions is the options for NewPollerBehaviorSimpleMaximum.
	//
	// NOTE: Experimental
	PollerBehaviorSimpleMaximumOptions = internal.PollerBehaviorSimpleMaximumOptions

	// WorkflowPanicPolicy is used for configuring how worker deals with workflow
	// code panicking which includes non backwards compatible changes to the workflow code without appropriate
	// versioning (see [workflow.GetVersion]).
	// The default behavior is to block workflow execution until the problem is fixed.
	WorkflowPanicPolicy = internal.WorkflowPanicPolicy

	// WorkflowReplayerOptions are options used for
	// NewWorkflowReplayerWithOptions.
	WorkflowReplayerOptions = internal.WorkflowReplayerOptions

	// ReplayWorkflowHistoryOptions are options for replaying a workflow.
	ReplayWorkflowHistoryOptions = internal.ReplayWorkflowHistoryOptions
)

var _ WorkflowRegistry = (WorkflowReplayer)(nil)

const (
	// BlockWorkflow is the default WorkflowPanicPolicy policy for handling workflow panics and detected non-determinism.
	// This option causes workflow to get stuck in the workflow task retry loop.
	// It is expected that after the problem is discovered and fixed the workflows are going to continue
	// without any additional manual intervention.
	BlockWorkflow = internal.BlockWorkflow
	// FailWorkflow WorkflowPanicPolicy immediately fails workflow execution if workflow code throws panic or
	// detects non-determinism. This feature is convenient during development.
	// WARNING: enabling this in production can cause all open workflows to fail on a single bug or bad deployment.
	FailWorkflow = internal.FailWorkflow
)

// New creates an instance of worker for managing workflow and activity executions.
//
//	client    - the client for use by the worker
//	taskQueue - is the task queue name you use to identify your client worker, also
//	           identifies group of workflow and activity implementations that are
//	           hosted by a single worker process
//	options  - configure any worker specific options like logger, metrics, identity
func New(
	client client.Client,
	taskQueue string,
	options Options,
) Worker {
	return internal.NewWorker(client, taskQueue, options)
}

// NewWorkflowReplayer creates a WorkflowReplayer instance.
func NewWorkflowReplayer() WorkflowReplayer {
	w, err := NewWorkflowReplayerWithOptions(WorkflowReplayerOptions{})
	if err != nil {
		panic(err)
	}
	return w
}

// NewWorkflowReplayerWithOptions creates a WorkflowReplayer instance with the
// given options.
func NewWorkflowReplayerWithOptions(options WorkflowReplayerOptions) (WorkflowReplayer, error) {
	return internal.NewWorkflowReplayer(options)
}

// EnableVerboseLogging enable or disable verbose logging of internal Temporal library components.
// Most customers don't need this feature, unless advised by the Temporal team member.
// Also there is no guarantee that this API is not going to change.
func EnableVerboseLogging(enable bool) {
	internal.EnableVerboseLogging(enable)
}

// SetStickyWorkflowCacheSize sets the cache size for sticky workflow cache. Sticky workflow execution is the affinity
// between workflow tasks of a specific workflow execution to a specific worker. The benefit of sticky execution is that
// the workflow does not have to reconstruct state by replaying history from the beginning. The cache is shared between
// workers running within same process. This must be called before any worker is started. If not called, the default
// size of 10K (which may change) will be used.
func SetStickyWorkflowCacheSize(cacheSize int) {
	internal.SetStickyWorkflowCacheSize(cacheSize)
}

// PurgeStickyWorkflowCache resets the sticky workflow cache. This must be called only when all workers are stopped.
func PurgeStickyWorkflowCache() {
	internal.PurgeStickyWorkflowCache()
}

// SetBinaryChecksum sets the identifier of the binary(aka BinaryChecksum).
// The identifier is mainly used in recording reset points when respondWorkflowTaskCompleted. For each workflow, the very first
// workflow task completed by a binary will be associated as a auto-reset point for the binary. So that when a customer wants to
// mark the binary as bad, the workflow will be reset to that point -- which means workflow will forget all progress generated
// by the binary.
// On another hand, once the binary is marked as bad, the bad binary cannot poll workflow queue and make any progress any more.
func SetBinaryChecksum(checksum string) {
	internal.SetBinaryChecksum(checksum)
}

// InterruptCh returns channel which will get data when system receives interrupt signal from OS. Pass it to worker.Run() func to stop worker with Ctrl+C.
func InterruptCh() <-chan interface{} {
	return internal.InterruptCh()
}

// NewPollerBehaviorSimpleMaximum creates a PollerBehavior that allows the worker to start up to a maximum number of pollers.
//
// NOTE: Experimental
func NewPollerBehaviorSimpleMaximum(
	options PollerBehaviorSimpleMaximumOptions,
) PollerBehavior {
	return internal.NewPollerBehaviorSimpleMaximum(options)
}

// NewPollerBehaviorAutoscaling creates a PollerBehavior that allows the worker to scale the number of pollers within a given range.
// based on the workflow and feedback from the server.
//
// NOTE: Experimental
func NewPollerBehaviorAutoscaling(
	options PollerBehaviorAutoscalingOptions,
) PollerBehavior {
	return internal.NewPollerBehaviorAutoscaling(options)
}
