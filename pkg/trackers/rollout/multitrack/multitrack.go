package multitrack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/werf/logboek/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/werf/kubedog/pkg/tracker"
	"github.com/werf/kubedog/pkg/tracker/canary"
	"github.com/werf/kubedog/pkg/tracker/daemonset"
	"github.com/werf/kubedog/pkg/tracker/debug"
	"github.com/werf/kubedog/pkg/tracker/deployment"
	"github.com/werf/kubedog/pkg/tracker/job"
	"github.com/werf/kubedog/pkg/tracker/statefulset"
)

type TrackTerminationMode string

const (
	WaitUntilResourceReady TrackTerminationMode = "WaitUntilResourceReady"
	NonBlocking            TrackTerminationMode = "NonBlocking"
)

type FailMode string

const (
	IgnoreAndContinueDeployProcess    FailMode = "IgnoreAndContinueDeployProcess"
	FailWholeDeployProcessImmediately FailMode = "FailWholeDeployProcessImmediately"
	HopeUntilEndOfDeployProcess       FailMode = "HopeUntilEndOfDeployProcess"
)

// type DeployCondition string
//
// const (
//	ControllerIsReady DeployCondition = "ControllerIsReady"
//	PodIsReady        DeployCondition = "PodIsReady"
//	EndOfDeploy       DeployCondition = "EndOfDeploy"
// )

var ErrFailWholeDeployProcessImmediately = errors.New("fail whole deploy process immediately")

type MultitrackSpecs struct {
	Deployments  []MultitrackSpec
	StatefulSets []MultitrackSpec
	DaemonSets   []MultitrackSpec
	Jobs         []MultitrackSpec
	Canaries     []MultitrackSpec
}

type MultitrackSpec struct {
	ResourceName string
	Namespace    string

	TrackTerminationMode    TrackTerminationMode
	FailMode                FailMode
	AllowFailuresCount      *int
	FailureThresholdSeconds *int

	IgnoreReadinessProbeFailsByContainerName map[string]time.Duration

	LogRegex                *regexp.Regexp
	LogRegexByContainerName map[string]*regexp.Regexp

	SkipLogs                  bool
	SkipLogsForContainers     []string
	ShowLogsOnlyForContainers []string
	// ShowLogsUntil             DeployCondition TODO

	ShowServiceMessages bool
}

type MultitrackOptions struct {
	tracker.Options
	StatusProgressPeriod time.Duration
}

func newMultitrackOptions(parentContext context.Context, timeout, statusProgessPeriod time.Duration, logsFromTime time.Time, ignoreReadinessProbeFailsByContainerName map[string]time.Duration) MultitrackOptions {
	return MultitrackOptions{
		Options: tracker.Options{
			ParentContext:                            parentContext,
			Timeout:                                  timeout,
			LogsFromTime:                             logsFromTime,
			IgnoreReadinessProbeFailsByContainerName: ignoreReadinessProbeFailsByContainerName,
		},
		StatusProgressPeriod: statusProgessPeriod,
	}
}

func setDefaultCanarySpecValues(spec *MultitrackSpec) {
	setDefaultSpecValues(spec)

	*spec.AllowFailuresCount = 0
}

func setDefaultSpecValues(spec *MultitrackSpec) {
	if spec.TrackTerminationMode == "" {
		spec.TrackTerminationMode = WaitUntilResourceReady
	}

	if spec.FailMode == "" {
		spec.FailMode = FailWholeDeployProcessImmediately
	}

	if spec.AllowFailuresCount == nil {
		spec.AllowFailuresCount = new(int)
		*spec.AllowFailuresCount = 1
	}

	if spec.FailureThresholdSeconds == nil {
		spec.FailureThresholdSeconds = new(int)
		*spec.FailureThresholdSeconds = 0
	}
}

func Multitrack(kube kubernetes.Interface, specs MultitrackSpecs, opts MultitrackOptions) error {
	if len(specs.Deployments)+len(specs.StatefulSets)+len(specs.DaemonSets)+len(specs.Jobs)+len(specs.Canaries) == 0 {
		return nil
	}

	for i := range specs.Deployments {
		//fmt.Println("遍历的deployments:", i)
		setDefaultSpecValues(&specs.Deployments[i])
	}

	for i := range specs.StatefulSets {
		setDefaultSpecValues(&specs.StatefulSets[i])
	}
	for i := range specs.DaemonSets {
		setDefaultSpecValues(&specs.DaemonSets[i])
	}
	for i := range specs.Jobs {
		setDefaultSpecValues(&specs.Jobs[i])
	}
	for i := range specs.Canaries {
		setDefaultCanarySpecValues(&specs.Canaries[i])
	}

	mt := multitracker{
		DeploymentsSpecs:        make(map[string]MultitrackSpec),
		DeploymentsContexts:     make(map[string]*multitrackerContext),
		TrackingDeployments:     make(map[string]*multitrackerResourceState),
		DeploymentsStatuses:     make(map[string]deployment.DeploymentStatus),
		PrevDeploymentsStatuses: make(map[string]deployment.DeploymentStatus),

		StatefulSetsSpecs:        make(map[string]MultitrackSpec),
		StatefulSetsContexts:     make(map[string]*multitrackerContext),
		TrackingStatefulSets:     make(map[string]*multitrackerResourceState),
		StatefulSetsStatuses:     make(map[string]statefulset.StatefulSetStatus),
		PrevStatefulSetsStatuses: make(map[string]statefulset.StatefulSetStatus),

		DaemonSetsSpecs:        make(map[string]MultitrackSpec),
		DaemonSetsContexts:     make(map[string]*multitrackerContext),
		TrackingDaemonSets:     make(map[string]*multitrackerResourceState),
		DaemonSetsStatuses:     make(map[string]daemonset.DaemonSetStatus),
		PrevDaemonSetsStatuses: make(map[string]daemonset.DaemonSetStatus),

		JobsSpecs:        make(map[string]MultitrackSpec),
		JobsContexts:     make(map[string]*multitrackerContext),
		TrackingJobs:     make(map[string]*multitrackerResourceState),
		JobsStatuses:     make(map[string]job.JobStatus),
		PrevJobsStatuses: make(map[string]job.JobStatus),

		CanariesSpecs:        make(map[string]MultitrackSpec),
		CanariesContexts:     make(map[string]*multitrackerContext),
		TrackingCanaries:     make(map[string]*multitrackerResourceState),
		CanariesStatuses:     make(map[string]canary.CanaryStatus),
		PrevCanariesStatuses: make(map[string]canary.CanaryStatus),

		serviceMessagesByResource: make(map[string][]string),
	}

	errorChan := make(chan error)
	doneChan := make(chan struct{})

	var statusProgressChan <-chan time.Time

	statusProgressPeriod := opts.StatusProgressPeriod
	if opts.StatusProgressPeriod == 0 {
		statusProgressPeriod = 5 * time.Second
	}

	if statusProgressPeriod > 0 {
		statusProgressTicker := time.NewTicker(statusProgressPeriod)
		defer statusProgressTicker.Stop()
		statusProgressChan = statusProgressTicker.C
	} else {
		statusProgressChan = make(chan time.Time)
	}

	doDisplayStatusProgress := func() error {
		mt.mux.Lock()
		defer mt.mux.Unlock()
		return mt.displayStatusProgress()
	}

	mt.Start(kube, specs, doneChan, errorChan, opts)

	for {
		select {
		case <-statusProgressChan:
			if err := doDisplayStatusProgress(); err != nil {
				return err
			}

		case <-doneChan:
			if debug.Debug() {
				//再来一次就退出
				doDisplayStatusProgress()
				fmt.Printf("-- Multitrack doneChan signal received => exiting\n")
			}

			return nil

		case err := <-errorChan:
			if err == nil {
				panic("unexpected nil error received through errorChan")
			}
			return err
		}
	}
}

func (mt *multitracker) Start(kube kubernetes.Interface, specs MultitrackSpecs, doneChan chan struct{}, errorChan chan error, opts MultitrackOptions) {
	mt.mux.Lock()
	defer mt.mux.Unlock()

	var wg sync.WaitGroup

	for _, spec := range specs.Deployments {
		mt.DeploymentsContexts[spec.ResourceName] = newMultitrackerContext(opts.ParentContext)
		mt.DeploymentsSpecs[spec.ResourceName] = spec
		mt.TrackingDeployments[spec.ResourceName] = newMultitrackerResourceState(spec)

		wg.Add(1)

		go mt.runSpecTracker("deploy", spec, mt.DeploymentsContexts[spec.ResourceName], &wg, mt.DeploymentsContexts, doneChan, errorChan, func(spec MultitrackSpec, mtCtx *multitrackerContext) error {
			return mt.TrackDeployment(kube, spec, newMultitrackOptions(mtCtx.Context, opts.Timeout, opts.StatusProgressPeriod, opts.LogsFromTime, spec.IgnoreReadinessProbeFailsByContainerName))
		})
	}

	for _, spec := range specs.StatefulSets {
		mt.StatefulSetsContexts[spec.ResourceName] = newMultitrackerContext(opts.ParentContext)
		mt.StatefulSetsSpecs[spec.ResourceName] = spec
		mt.TrackingStatefulSets[spec.ResourceName] = newMultitrackerResourceState(spec)

		wg.Add(1)

		go mt.runSpecTracker("sts", spec, mt.StatefulSetsContexts[spec.ResourceName], &wg, mt.StatefulSetsContexts, doneChan, errorChan, func(spec MultitrackSpec, mtCtx *multitrackerContext) error {
			return mt.TrackStatefulSet(kube, spec, newMultitrackOptions(mtCtx.Context, opts.Timeout, opts.StatusProgressPeriod, opts.LogsFromTime, spec.IgnoreReadinessProbeFailsByContainerName))
		})
	}

	for _, spec := range specs.DaemonSets {
		mt.DaemonSetsContexts[spec.ResourceName] = newMultitrackerContext(opts.ParentContext)
		mt.DaemonSetsSpecs[spec.ResourceName] = spec
		mt.TrackingDaemonSets[spec.ResourceName] = newMultitrackerResourceState(spec)

		wg.Add(1)

		go mt.runSpecTracker("ds", spec, mt.DaemonSetsContexts[spec.ResourceName], &wg, mt.DaemonSetsContexts, doneChan, errorChan, func(spec MultitrackSpec, mtCtx *multitrackerContext) error {
			return mt.TrackDaemonSet(kube, spec, newMultitrackOptions(mtCtx.Context, opts.Timeout, opts.StatusProgressPeriod, opts.LogsFromTime, spec.IgnoreReadinessProbeFailsByContainerName))
		})
	}

	for _, spec := range specs.Jobs {
		mt.JobsContexts[spec.ResourceName] = newMultitrackerContext(opts.ParentContext)
		mt.JobsSpecs[spec.ResourceName] = spec
		mt.TrackingJobs[spec.ResourceName] = newMultitrackerResourceState(spec)

		wg.Add(1)

		go mt.runSpecTracker("job", spec, mt.JobsContexts[spec.ResourceName], &wg, mt.JobsContexts, doneChan, errorChan, func(spec MultitrackSpec, mtCtx *multitrackerContext) error {
			return mt.TrackJob(kube, spec, newMultitrackOptions(mtCtx.Context, opts.Timeout, opts.StatusProgressPeriod, opts.LogsFromTime, spec.IgnoreReadinessProbeFailsByContainerName))
		})
	}

	for _, spec := range specs.Canaries {
		mt.CanariesContexts[spec.ResourceName] = newMultitrackerContext(opts.ParentContext)
		mt.CanariesSpecs[spec.ResourceName] = spec
		mt.TrackingCanaries[spec.ResourceName] = newMultitrackerResourceState(spec)

		wg.Add(1)

		go mt.runSpecTracker("canary", spec, mt.CanariesContexts[spec.ResourceName], &wg, mt.CanariesContexts, doneChan, errorChan, func(spec MultitrackSpec, mtCtx *multitrackerContext) error {
			return mt.TrackCanary(kube, spec, newMultitrackOptions(mtCtx.Context, opts.Timeout, opts.StatusProgressPeriod, opts.LogsFromTime, spec.IgnoreReadinessProbeFailsByContainerName))
		})
	}

	if err := mt.applyTrackTerminationMode(); err != nil {
		errorChan <- fmt.Errorf("unable to apply termination mode: %s", err)
		return
	}

	go func() {
		wg.Wait()

		isAlreadyFailed := func() bool {
			mt.mux.Lock()
			defer mt.mux.Unlock()
			return mt.isFailed
		}()
		if isAlreadyFailed {
			return
		}

		err := func() error {
			mt.mux.Lock()
			defer mt.mux.Unlock()
			return mt.displayStatusProgress()
		}()
		if err != nil {
			errorChan <- err
			return
		}

		if mt.hasFailedTrackingResources() {
			mt.displayFailedTrackingResourcesServiceMessages()
			errorChan <- mt.formatFailedTrackingResourcesError()
		} else {
			if debug.Debug() {
				fmt.Printf("-- Multitrack send doneChan signal from workgroup wait goroutine\n")
			}

			doneChan <- struct{}{}
		}
	}()
}

func (mt *multitracker) applyTrackTerminationMode() error {
	if mt.isTerminating {
		return nil
	}

	shouldContinueTracking := func(name string, spec MultitrackSpec) bool {
		switch spec.TrackTerminationMode {
		case WaitUntilResourceReady:
			// There is at least one active context with wait mode,
			// so continue tracking without stopping any contexts
			return true

		case NonBlocking:
			return false

		default:
			panic(fmt.Sprintf("unknown TrackTerminationMode %#v", spec.TrackTerminationMode))
		}
	}

	var contextsToStop []*multitrackerContext
	var debugMsg []string

	for name, ctx := range mt.DeploymentsContexts {
		if shouldContinueTracking(name, mt.DeploymentsSpecs[name]) {
			return nil
		}
		debugMsg = append(debugMsg, fmt.Sprintf("will stop context for deployment %q", name))
		contextsToStop = append(contextsToStop, ctx)
	}
	for name, ctx := range mt.StatefulSetsContexts {
		if shouldContinueTracking(name, mt.StatefulSetsSpecs[name]) {
			return nil
		}
		debugMsg = append(debugMsg, fmt.Sprintf("will stop context for sts %q", name))
		contextsToStop = append(contextsToStop, ctx)
	}
	for name, ctx := range mt.DaemonSetsContexts {
		if shouldContinueTracking(name, mt.DaemonSetsSpecs[name]) {
			return nil
		}
		debugMsg = append(debugMsg, fmt.Sprintf("will stop context for ds %q", name))
		contextsToStop = append(contextsToStop, ctx)
	}
	for name, ctx := range mt.JobsContexts {
		if shouldContinueTracking(name, mt.JobsSpecs[name]) {
			return nil
		}
		debugMsg = append(debugMsg, fmt.Sprintf("will stop context for job %q", name))
		contextsToStop = append(contextsToStop, ctx)
	}
	for name, ctx := range mt.CanariesContexts {
		if shouldContinueTracking(name, mt.CanariesSpecs[name]) {
			return nil
		}
		contextsToStop = append(contextsToStop, ctx)
	}

	mt.isTerminating = true

	if debug.Debug() {
		for _, msg := range debugMsg {
			fmt.Printf("-- applyTrackTerminationMode: %s\n", msg)
		}
	}

	for _, ctx := range contextsToStop {
		ctx.CancelFunc()
	}

	return nil
}

func (mt *multitracker) runSpecTracker(kind string, spec MultitrackSpec, mtCtx *multitrackerContext, wg *sync.WaitGroup, contexts map[string]*multitrackerContext, doneChan chan struct{}, errorChan chan error, trackerFunc func(MultitrackSpec, *multitrackerContext) error) {
	defer wg.Done()

	err := trackerFunc(spec, mtCtx)

	mt.mux.Lock()
	defer mt.mux.Unlock()

	delete(contexts, spec.ResourceName)

	if err == ErrFailWholeDeployProcessImmediately {
		mt.displayFailedTrackingResourcesServiceMessages()
		errorChan <- mt.formatFailedTrackingResourcesError()
		mt.isFailed = true
		return
	} else if err != nil {
		// unknown error
		errorChan <- fmt.Errorf("%s/%s track failed: %s", kind, spec.ResourceName, err)
		mt.isFailed = true
		return
	}

	if err := mt.applyTrackTerminationMode(); err != nil {
		errorChan <- fmt.Errorf("unable to apply termination mode: %s", err)
		mt.isFailed = true
		return
	}
}

type multitracker struct {
	DeploymentsSpecs        map[string]MultitrackSpec
	DeploymentsContexts     map[string]*multitrackerContext
	TrackingDeployments     map[string]*multitrackerResourceState
	DeploymentsStatuses     map[string]deployment.DeploymentStatus
	PrevDeploymentsStatuses map[string]deployment.DeploymentStatus

	StatefulSetsSpecs        map[string]MultitrackSpec
	StatefulSetsContexts     map[string]*multitrackerContext
	TrackingStatefulSets     map[string]*multitrackerResourceState
	StatefulSetsStatuses     map[string]statefulset.StatefulSetStatus
	PrevStatefulSetsStatuses map[string]statefulset.StatefulSetStatus

	DaemonSetsSpecs        map[string]MultitrackSpec
	DaemonSetsContexts     map[string]*multitrackerContext
	TrackingDaemonSets     map[string]*multitrackerResourceState
	DaemonSetsStatuses     map[string]daemonset.DaemonSetStatus
	PrevDaemonSetsStatuses map[string]daemonset.DaemonSetStatus

	JobsSpecs        map[string]MultitrackSpec
	JobsContexts     map[string]*multitrackerContext
	TrackingJobs     map[string]*multitrackerResourceState
	JobsStatuses     map[string]job.JobStatus
	PrevJobsStatuses map[string]job.JobStatus

	CanariesSpecs        map[string]MultitrackSpec
	CanariesContexts     map[string]*multitrackerContext
	TrackingCanaries     map[string]*multitrackerResourceState
	CanariesStatuses     map[string]canary.CanaryStatus
	PrevCanariesStatuses map[string]canary.CanaryStatus

	mux sync.Mutex

	isFailed      bool
	isTerminating bool

	displayCalled             bool
	currentLogProcessHeader   string
	currentLogProcess         types.LogProcessInterface
	serviceMessagesByResource map[string][]string
}

type multitrackerContext struct {
	Context    context.Context
	CancelFunc context.CancelFunc
}

func newMultitrackerContext(parentContext context.Context) *multitrackerContext {
	if parentContext == nil {
		parentContext = context.Background()
	}
	ctx, cancel := context.WithCancel(parentContext)
	return &multitrackerContext{Context: ctx, CancelFunc: cancel}
}

type multitrackerResourceStatus string

const (
	resourceActive            multitrackerResourceStatus = "resourceActive"
	resourceSucceeded         multitrackerResourceStatus = "resourceSucceeded"
	resourceFailed            multitrackerResourceStatus = "resourceFailed"
	resourceHoping            multitrackerResourceStatus = "resourceHoping"
	resourceActiveAfterHoping multitrackerResourceStatus = "resourceActiveAfterHoping"
)

type multitrackerResourceState struct {
	Status                   multitrackerResourceStatus
	FailedReason             string
	FailuresCount            int
	FailuresCountAfterHoping int
}

func newMultitrackerResourceState(spec MultitrackSpec) *multitrackerResourceState {
	return &multitrackerResourceState{Status: resourceActive}
}

func (mt *multitracker) hasFailedTrackingResources() bool {
	for _, states := range []map[string]*multitrackerResourceState{
		mt.TrackingDeployments,
		mt.TrackingStatefulSets,
		mt.TrackingDaemonSets,
		mt.TrackingJobs,
	} {
		for _, state := range states {
			if state.Status == resourceFailed {
				return true
			}
		}
	}
	return false
}

func (mt *multitracker) formatFailedTrackingResourcesError() error {
	msgParts := []string{}

	for name, state := range mt.TrackingDeployments {
		if state.Status != resourceFailed {
			continue
		}
		msgParts = append(msgParts, fmt.Sprintf("deploy/%s failed: %s", name, state.FailedReason))
	}
	for name, state := range mt.TrackingStatefulSets {
		if state.Status != resourceFailed {
			continue
		}
		msgParts = append(msgParts, fmt.Sprintf("sts/%s failed: %s", name, state.FailedReason))
	}
	for name, state := range mt.TrackingDaemonSets {
		if state.Status != resourceFailed {
			continue
		}
		msgParts = append(msgParts, fmt.Sprintf("ds/%s failed: %s", name, state.FailedReason))
	}
	for name, state := range mt.TrackingJobs {
		if state.Status != resourceFailed {
			continue
		}
		msgParts = append(msgParts, fmt.Sprintf("job/%s failed: %s", name, state.FailedReason))
	}
	for name, state := range mt.TrackingCanaries {
		if state.Status != resourceFailed {
			continue
		}
		msgParts = append(msgParts, fmt.Sprintf("canary/%s failed: %s", name, state.FailedReason))
	}

	return fmt.Errorf("%s", strings.Join(msgParts, "\n"))
}

func (mt *multitracker) handleResourceReadyCondition(resourcesStates map[string]*multitrackerResourceState, spec MultitrackSpec) error {
	resourcesStates[spec.ResourceName].Status = resourceSucceeded
	return tracker.StopTrack
}

func (mt *multitracker) handleResourceFailure(resourcesStates map[string]*multitrackerResourceState, kind string, spec MultitrackSpec, reason string) error {
	forceFailure := false
	if strings.Contains(reason, "ErrImageNeverPull") {
		forceFailure = true
	}

	switch spec.FailMode {
	case FailWholeDeployProcessImmediately:
		resourcesStates[spec.ResourceName].FailuresCount++

		if !forceFailure && resourcesStates[spec.ResourceName].FailuresCount <= *spec.AllowFailuresCount {
			mt.displayMultitrackServiceMessageF("%d/%d allowed errors occurred for %s/%s: continue tracking\n", resourcesStates[spec.ResourceName].FailuresCount, *spec.AllowFailuresCount, kind, spec.ResourceName)
			return nil
		}

		if forceFailure {
			mt.displayMultitrackServiceMessageF("Critical failure for %s/%s has been occurred: stop tracking immediately!\n", kind, spec.ResourceName, *spec.AllowFailuresCount)
		} else {
			mt.displayMultitrackServiceMessageF("Allowed failures count for %s/%s exceeded %d errors: stop tracking immediately!\n", kind, spec.ResourceName, *spec.AllowFailuresCount)
		}

		resourcesStates[spec.ResourceName].Status = resourceFailed
		resourcesStates[spec.ResourceName].FailedReason = reason

		return ErrFailWholeDeployProcessImmediately

	case HopeUntilEndOfDeployProcess:

	handleResourceState:
		switch resourcesStates[spec.ResourceName].Status {
		case resourceActive:
			resourcesStates[spec.ResourceName].Status = resourceHoping
			goto handleResourceState

		case resourceHoping:
			activeResourcesNames := mt.getActiveResourcesNames()
			if len(activeResourcesNames) > 0 {
				mt.displayMultitrackServiceMessageF("Error occurred for %s/%s, waiting until following resources are ready before counting errors (HopeUntilEndOfDeployProcess fail mode is active): %s\n", kind, spec.ResourceName, strings.Join(activeResourcesNames, ", "))
				return nil
			}

			resourcesStates[spec.ResourceName].Status = resourceActiveAfterHoping
			goto handleResourceState

		case resourceActiveAfterHoping:
			resourcesStates[spec.ResourceName].FailuresCount++

			if resourcesStates[spec.ResourceName].FailuresCount <= *spec.AllowFailuresCount {
				mt.displayMultitrackServiceMessageF("%d/%d allowed errors occurred for %s/%s: continue tracking\n", resourcesStates[spec.ResourceName].FailuresCount, *spec.AllowFailuresCount, kind, spec.ResourceName)
				return nil
			}

			mt.displayMultitrackServiceMessageF("Allowed failures count for %s/%s exceeded %d errors: stop tracking immediately!\n", kind, spec.ResourceName, *spec.AllowFailuresCount)

			resourcesStates[spec.ResourceName].Status = resourceFailed
			resourcesStates[spec.ResourceName].FailedReason = reason

			return ErrFailWholeDeployProcessImmediately

		default:
			panic(fmt.Sprintf("%s/%s tracker is in unexpected state %#v", kind, spec.ResourceName, resourcesStates[spec.ResourceName].Status))
		}

	case IgnoreAndContinueDeployProcess:
		resourcesStates[spec.ResourceName].FailuresCount++
		mt.displayMultitrackServiceMessageF("%d errors occurred for %s/%s\n", resourcesStates[spec.ResourceName].FailuresCount, kind, spec.ResourceName)
		return nil

	default:
		panic(fmt.Sprintf("bad fail mode %#v for resource %s/%s", spec.FailMode, kind, spec.ResourceName))
	}
}

func (mt *multitracker) getActiveResourcesNames() []string {
	activeResources := []string{}

	for name, state := range mt.TrackingDeployments {
		if state.Status == resourceActive {
			activeResources = append(activeResources, fmt.Sprintf("deploy/%s", name))
		}
	}
	for name, state := range mt.TrackingStatefulSets {
		if state.Status == resourceActive {
			activeResources = append(activeResources, fmt.Sprintf("sts/%s", name))
		}
	}
	for name, state := range mt.TrackingDaemonSets {
		if state.Status == resourceActive {
			activeResources = append(activeResources, fmt.Sprintf("ds/%s", name))
		}
	}
	for name, state := range mt.TrackingJobs {
		if state.Status == resourceActive {
			activeResources = append(activeResources, fmt.Sprintf("job/%s", name))
		}
	}

	return activeResources
}
