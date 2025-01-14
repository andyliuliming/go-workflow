package flow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/benbjohnson/clock"
)

// Workflow represents a collection of connected Steps that form a directed acyclic graph (DAG).
//
// The Steps are connected via dependency, use Step(), Steps() or Pipe() to add Steps into Workflow.
//
//	workflow.Add(
//		Step(a),
//		Steps(b, c).DependsOn(a),	// a -> b, c
//		Pipe(d, e, f),              // d -> e -> f
//	)
//
// Workflow will execute Steps in a topological order, each Step will be executed in a separate goroutine.
// Workflow guarantees that
//
//	Before a Step goroutine starts, all its Upstream Steps are terminated, and registered Input callbacks are called.
//
// Workflow supports Step-level configuration,       check Step(), Steps() and Pipe() for details.
// Workflow supports Workflow-level configuration,   check WorkflowOption for details.
// Workflow supports executing Steps phase in phase, check Phase for details.
// Workflow supports Nested Steps,				     check Is(), As() and StepTree for details.
type Workflow struct {
	tree  StepTree              // tree of Nested / Wrapped Steps, only root Steps are used in the below fields
	state map[Steper]*State     // the internal states of Steps
	steps map[Phase]Set[Steper] // all Steps grouped in phases

	leaseBucket       chan struct{}  // constraint max concurrency of running Steps
	waitGroup         sync.WaitGroup // to prevent goroutine leak
	isRunning         sync.Mutex     // indicate whether the Workflow is running
	oneStepTerminated chan struct{}  // signals for next tick
	clock             clock.Clock    // clock for unit test
	notify            []Notify       // notify before and after Step
	DontPanic         bool           // whether recover panic from Step(s)
}

// Add Steps into Workflow in phase Main.
func (w *Workflow) Add(was ...WorkflowAdder) *Workflow { return w.PhaseAdd(PhaseMain, was...) }

// Init adds Steps into Workflow in phase Init.
func (w *Workflow) Init(was ...WorkflowAdder) *Workflow { return w.PhaseAdd(PhaseInit, was...) }

// Defer adds Steps into Workflow in phase Defer.
func (w *Workflow) Defer(was ...WorkflowAdder) *Workflow { return w.PhaseAdd(PhaseDefer, was...) }

// PhaseAdd add Steps into specific phase.
func (w *Workflow) PhaseAdd(phase Phase, was ...WorkflowAdder) *Workflow {
	if w.tree == nil {
		w.tree = make(StepTree)
	}
	if w.state == nil {
		w.state = make(map[Steper]*State)
	}
	if w.steps == nil {
		w.steps = make(map[Phase]Set[Steper])
	}
	if w.steps[phase] == nil {
		w.steps[phase] = make(Set[Steper])
	}
	for _, wa := range was {
		if wa != nil {
			for step, config := range wa.Done() {
				w.addStep(phase, step, config)
			}
		}
	}
	return w
}

// AddStep adds a Step into Workflow with the given phase and config.
func (w *Workflow) addStep(phase Phase, step Steper, config *StepConfig) {
	if step == nil {
		return
	}
	w.steps[phase].Add(step)
	if w.StateOf(step) == nil {
		// the step is new, it becomes a new root
		w.state[step] = new(State)
		// add the new root (and all its descendant steps) to the tree,
		// tree.Add() returns all old roots in descendant steps.
		// we need to replace them with the new root.
		// workflow will only orchestrate the root Steps,
		// and leave the descendant Steps being managed by the root Steps.
		for old := range w.tree.Add(step) {
			w.state[step].MergeConfig(w.state[old].Config)
			delete(w.state, old)
			for _, phase := range w.steps {
				if phase != nil && phase.Has(old) {
					phase.Add(step)
					delete(phase, old)
				}
			}
		}
	}
	if config != nil {
		for up := range config.Upstreams {
			w.setUpstream(phase, step, up)
		}
		config.Upstreams = nil
		// merge config to the state in the lowest workflow
		w.StateOf(step).MergeConfig(config)
	}
}

// setUpstreams will put the upstreams into proper state.
func (w *Workflow) setUpstream(phase Phase, step, up Steper) {
	if step == nil || up == nil {
		return
	}
	// just add the upstream step to the phase
	// even upstream already in, we still need add it to the phase
	w.addStep(phase, up, nil)
	if w.StateOf(up) == nil { // the upstream is not in the Workflow
		w.StateOf(w.RootOf(step)).AddUpstream(up)
		return
	}
	// find the lowest workflow manages both step and up
	ancestor := w.tree[step]
	for {
		if ancestor == nil {
			return
		}
		if s, ok := ancestor.(interface {
			StateOf(Steper) *State
			RootOf(Steper) Steper
		}); ok {
			if s.StateOf(up) != nil {
				s.StateOf(s.RootOf(step)).AddUpstream(up)
				return
			}
		}
		if w.tree.IsRoot(ancestor) {
			break
		}
		ancestor = w.tree[ancestor]
	}
	w.StateOf(ancestor).AddUpstream(up)
}

func (w *Workflow) empty() bool { return len(w.tree) == 0 || len(w.state) == 0 || len(w.steps) == 0 }

// Steps returns all root Steps in the Workflow.
func (w *Workflow) Steps() []Steper { return w.Unwrap() }
func (w *Workflow) Unwrap() []Steper {
	if w.empty() {
		return nil
	}
	rv := []Steper{}
	for step := range w.state {
		rv = append(rv, step)
	}
	return rv
}

// RootOf returns the root Step of the given Step.
func (w *Workflow) RootOf(step Steper) Steper {
	if w.empty() {
		return nil
	}
	return w.tree.RootOf(step)
}

// StateOf returns the internal state of the Step.
// State includes Step's status, error, input, dependency and config.
func (w *Workflow) StateOf(step Steper) *State {
	if w.empty() || step == nil || w.tree[step] == nil {
		return nil
	}
	ancestor := w.tree[step]
	if ancestor == step { // the current step is a root
		return w.state[step]
	}
	// check whether the lowest ancestor implements StateOf().
	// normally, the ancestor should be a nested Workflow managing the step.
	// returning the state of the step is useful when
	// 1. we could know the exact status or error
	// 2. we could update the input to the step directly instead of its wrapped Workflow
	if s, ok := ancestor.(interface{ StateOf(Steper) *State }); ok {
		return s.StateOf(step)
	}
	// otherwise, track back to the root
	return w.state[w.RootOf(ancestor)]
}

// PhaseOf returns the execution phase of the Step.
func (w *Workflow) PhaseOf(step Steper) Phase {
	if w.empty() {
		return PhaseUnknown
	}
	root := w.RootOf(step)
	for _, phase := range WorkflowPhases {
		if steps := w.steps[phase]; steps != nil {
			if steps.Has(root) {
				return phase
			}
		}
	}
	return PhaseUnknown
}

// UpstreamOf returns all upstream Steps of the Step.
// Upstream Steps are the Steps that the Step depends on.
func (w *Workflow) UpstreamOf(step Steper) map[Steper]StatusError {
	if w.empty() {
		return nil
	}
	root := w.RootOf(step)
	rv := make(map[Steper]StatusError)
	for _, phase := range WorkflowPhases {
		if steps := w.steps[phase]; steps != nil {
			if steps.Has(root) {
				for up := range w.StateOf(root).Upstreams() {
					up = w.RootOf(up)
					rv[up] = w.StateOf(up).GetStatusError()
				}
			}
		}
	}
	return rv
}

// DownstreamOf returns all downstream Steps of the Step.
// Downstream Steps are the Steps that depend on the Step.
func (w *Workflow) DownstreamOf(step Steper) map[Steper]StatusError {
	if w.empty() {
		return nil
	}
	root := w.tree[step]
	rv := make(map[Steper]StatusError)
	for _, phase := range WorkflowPhases {
		for down := range w.steps[phase] {
			for up := range w.StateOf(down).Upstreams() {
				if w.RootOf(up) == root {
					rv[down] = w.StateOf(down).GetStatusError()
				}
			}
		}
	}
	return rv
}

// IsTerminated returns true if all Steps terminated.
func (w *Workflow) IsTerminated() bool {
	for _, phase := range WorkflowPhases {
		if !w.IsPhaseTerminated(phase) {
			return false
		}
	}
	return true
}
func (w *Workflow) IsPhaseTerminated(phase Phase) bool {
	if w.empty() {
		return true
	}
	for step := range w.steps[phase] {
		if !w.StateOf(step).GetStatus().IsTerminated() {
			return false
		}
	}
	return true
}

// Do starts the Step execution in topological order,
// and waits until all Steps terminated.
//
// Do will block the current goroutine.
func (w *Workflow) Do(ctx context.Context) error {
	// assert the Workflow is not running
	if !w.isRunning.TryLock() {
		return ErrWorkflowIsRunning
	}
	defer w.isRunning.Unlock()
	// if no steps to run
	if w.empty() {
		return nil
	}
	// preflight check
	if err := w.preflight(); err != nil {
		return err
	}
	// new fields for ready to tick
	if w.clock == nil {
		w.clock = clock.New()
	}
	w.oneStepTerminated = make(chan struct{}, len(w.state)+1) // need one more for the first tick
	// signal for the first tick
	w.signalTick()
	// each time one Step terminated, tick forward
	for range w.oneStepTerminated {
		if done := w.tick(ctx); done {
			break
		}
	}
	// ensure all goroutines are exited
	w.waitGroup.Wait()
	// return the error
	err := make(ErrWorkflow)
	for step, state := range w.state {
		err[step] = state.GetStatusError()
	}
	if err.IsNil() {
		return nil
	}
	return err
}

const scanned StepStatus = "scanned" // a private status for preflight
func isAllUpstreamScanned(ups map[Steper]StatusError) bool {
	for _, up := range ups {
		if up.Status != scanned {
			return false
		}
	}
	return true
}
func isAnyUpstreamNotTerminated(ups map[Steper]StatusError) bool {
	for _, up := range ups {
		if !up.Status.IsTerminated() {
			return true
		}
	}
	return false
}
func (w *Workflow) preflight() error {
	// assert all Steps' status start with Pending
	unexpectStatusSteps := make(ErrUnexpectStepInitStatus)
	for step, state := range w.state {
		if status := state.GetStatus(); status != Pending {
			unexpectStatusSteps[step] = status
		}
	}
	if len(unexpectStatusSteps) > 0 {
		return unexpectStatusSteps
	}
	// assert all dependency would not form a cycle
	// start scanning, mark Step as Scanned only when its all depdencies are Scanned
	for {
		hasNewScanned := false // whether a new Step being marked as Scanned this turn
		for step, state := range w.state {
			if state.GetStatus() == scanned {
				continue
			}
			if isAllUpstreamScanned(w.UpstreamOf(step)) {
				hasNewScanned = true
				state.SetStatus(scanned)
			}
		}
		if !hasNewScanned { // break when no new Step being Scanned
			break
		}
	}
	// check whether still have Steps not Scanned,
	// not Scanned Steps are in a cycle.
	stepsInCycle := make(ErrCycleDependency)
	for step, state := range w.state {
		if state.GetStatus() == scanned {
			continue
		}
		for up, statusErr := range w.UpstreamOf(step) {
			if statusErr.Status != scanned {
				stepsInCycle[step] = append(stepsInCycle[step], up)
			}
		}
	}
	if len(stepsInCycle) > 0 {
		return stepsInCycle
	}
	// reset all Steps' status to Pending
	for _, step := range w.state {
		step.SetStatus(Pending)
	}
	return nil
}

func (w *Workflow) signalTick() { w.oneStepTerminated <- struct{}{} }

// tick will not block, it starts a goroutine for each runnable Step.
// tick returns true if all steps in all phases are terminated.
func (w *Workflow) tick(ctx context.Context) bool {
	var steps Set[Steper]
	for _, phase := range WorkflowPhases {
		if !w.IsPhaseTerminated(phase) {
			steps = w.steps[phase]
			break
		}
	}
	if steps == nil {
		return true
	}
	for step := range steps {
		state := w.StateOf(step)
		// continue if the Step is not Pending
		if state.GetStatus() != Pending {
			continue
		}
		// continue if any Upstream is not terminated
		ups := w.UpstreamOf(step)
		if isAnyUpstreamNotTerminated(ups) {
			continue
		}
		option := state.Option()
		cond := DefaultCondition
		if option != nil && option.Condition != nil {
			cond = option.Condition
		}
		if nextStatus := cond(ctx, ups); nextStatus.IsTerminated() {
			state.SetStatus(nextStatus)
			w.signalTick()
			continue
		}
		// start the Step
		w.lease()
		state.SetStatus(Running)
		w.waitGroup.Add(1)
		go func(ctx context.Context, step Steper, state *State) {
			defer w.waitGroup.Done()
			defer w.signalTick()
			defer w.unlease()

			err := w.runStep(ctx, step, state)
			var result StepStatus
			switch {
			case err == nil:
				result = Succeeded
			case DefaultIsCanceled(err):
				result = Canceled
			case errors.Is(err, &ErrSkip{}):
				result = Skipped
			default:
				result = Failed
			}
			state.SetStatus(result)
			state.SetError(err)
		}(ctx, step, state)
	}
	return false
}

func (w *Workflow) runStep(ctx context.Context, step Steper, state *State) error {
	// set Step-level timeout for the Step
	var notAfter time.Time
	option := state.Option()
	if option != nil && option.Timeout != nil {
		notAfter = w.clock.Now().Add(*option.Timeout)
		var cancel func()
		ctx, cancel = w.clock.WithDeadline(ctx, notAfter)
		defer cancel()
	}
	// run the Step with or without retry
	do := w.makeDoForStep(step, state)
	return w.retry(option.RetryOption)(ctx, do, notAfter)
}

// makeDoForStep is panic-free from Step's Do and Input.
func (w *Workflow) makeDoForStep(step Steper, state *State) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		do := func(fn func() error) error { return fn() }
		if w.DontPanic {
			do = catchPanicAsError
		}
		return do(func() error {
			var err error
			ctx, afterStep := w.notifyStep(ctx, step)
			defer func() {
				afterStep(ctx, step, err)
			}()
			// apply up's output to current Step's input
			if ierr := do(func() error {
				return state.Input(ctx)
			}); ierr != nil {
				err = ErrInput{Err: ierr}
				return err
			}
			err = step.Do(ctx)
			return err
		})
	}
}
func (w *Workflow) notifyStep(ctx context.Context, step Steper) (context.Context, func(context.Context, Steper, error)) {
	afterStep := []func(context.Context, Steper, error){}
	for _, notify := range w.notify {
		if notify.BeforeStep != nil {
			ctx = notify.BeforeStep(ctx, step)
		}
		if notify.AfterStep != nil {
			afterStep = append(afterStep, notify.AfterStep)
		}
	}
	return ctx, func(ctx context.Context, sr Steper, err error) {
		for _, notify := range afterStep {
			notify(ctx, sr, err)
		}
	}
}
func (w *Workflow) lease() {
	if w.leaseBucket != nil {
		w.leaseBucket <- struct{}{}
	}
}
func (w *Workflow) unlease() {
	if w.leaseBucket != nil {
		<-w.leaseBucket
	}
}

// catchPanicAsError catches panic from f and return it as error.
func catchPanicAsError(f func() error) error {
	var returnErr error
	func(err *error) {
		defer func() {
			if r := recover(); r != nil {
				switch t := r.(type) {
				case error:
					*err = t
				default:
					*err = fmt.Errorf("%s", r)
				}
				*err = ErrPanic{Err: *err}
			}
		}()
		*err = f()
	}(&returnErr)
	return returnErr
}
