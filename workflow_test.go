package flow

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDep(t *testing.T) {
	a := Func("A", func(ctx context.Context) error { return nil })
	b := Func("B", func(ctx context.Context) error { return nil })
	c := Func("C", func(ctx context.Context) error { return nil })
	d := Func("D", func(ctx context.Context) error { return nil })
	t.Run("(a -> b, c) (c -> d)", func(t *testing.T) {
		workflow := new(Workflow)
		workflow.Add(
			Step(a).DependsOn(b, c),
			Step(c).DependsOn(d),
		)
		t.Run("list all steps from dependency", func(t *testing.T) {
			t.Parallel()
			var dep []Steper
			for s := range workflow.Steps {
				dep = append(dep, s)
			}
			assert.ElementsMatch(t, []Steper{a, b, c, d}, dep)
		})
		t.Run("list all upstream of some step", func(t *testing.T) {
			t.Parallel()
			assert.ElementsMatch(t, []Steper{b, c}, keys(workflow.UpstreamOf(a)))
			assert.ElementsMatch(t, []Steper{}, keys(workflow.UpstreamOf(b)))
			assert.ElementsMatch(t, []Steper{d}, keys(workflow.UpstreamOf(c)))
			assert.ElementsMatch(t, []Steper{}, keys(workflow.UpstreamOf(d)))
		})
		t.Run("list all downstrem of some step", func(t *testing.T) {
			t.Parallel()
			assert.ElementsMatch(t, []Steper{}, keys(workflow.DownstreamOf(a)))
			assert.ElementsMatch(t, []Steper{a}, keys(workflow.DownstreamOf(b)))
			assert.ElementsMatch(t, []Steper{a}, keys(workflow.DownstreamOf(c)))
			assert.ElementsMatch(t, []Steper{c}, keys(workflow.DownstreamOf(d)))
		})
	})
	t.Run("cycle dependency", func(t *testing.T) {
		workflow := new(Workflow)
		workflow.Add(
			Step(a).DependsOn(b),
			Step(b).DependsOn(c),
			Step(c).DependsOn(a),
		)
		var err ErrCycleDependency
		assert.ErrorAs(t, workflow.Do(context.Background()), &err)
		assert.Len(t, err, 3)
	})
}

func TestPreflight(t *testing.T) {
	t.Run("WorkflowIsRunning", func(t *testing.T) {
		t.Parallel()
		start := make(chan struct{})
		done := make(chan struct{})
		blockUntilDone := Func("block until done", func(ctx context.Context) error {
			start <- struct{}{}
			<-done
			return nil
		})
		workflow := new(Workflow)
		workflow.Add(
			Step(blockUntilDone),
		)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			workflow.Do(context.Background())
		}()

		// ensure step is running
		<-start
		assert.ErrorIs(t, workflow.Do(context.Background()), ErrWorkflowIsRunning)

		// unblock step
		close(done)

		// wait workflow to finish
		wg.Wait()
	})
	t.Run("empty Workflow will just return nil", func(t *testing.T) {
		t.Parallel()
		workflow := new(Workflow)
		assert.NoError(t, workflow.Do(context.Background()))
		assert.NoError(t, workflow.Do(context.Background()))
	})
	t.Run("Workflow has run", func(t *testing.T) {
		t.Parallel()
		workflow := new(Workflow)
		workflow.Add(Step(Func("A", func(ctx context.Context) error { return nil })))
		assert.NoError(t, workflow.Do(context.Background()))
		assert.ErrorIs(t, workflow.Do(context.Background()), ErrWorkflowHasRun)
	})
}

func TestWorkflowWillRecover(t *testing.T) {
	t.Run("panic in step", func(t *testing.T) {
		t.Parallel()
		workflow := new(Workflow)
		panicStep := Func("panic", func(ctx context.Context) error {
			panic("panic in step")
		})
		workflow.Add(
			Step(panicStep),
		)
		err := workflow.Do(context.Background())
		assert.ErrorContains(t, err, "panic in step")
	})
	t.Run("panic in flow", func(t *testing.T) {
		t.Parallel()
		workflow := new(Workflow)
		answer := FuncO("answer", func(ctx context.Context) (int, error) {
			return 42, nil
		})
		print := FuncI("print", func(ctx context.Context, msg string) error {
			fmt.Println(msg)
			return nil
		})

		workflow.Add(
			Step(print).
				InputDependsOn(Adapt(answer,
					func(ctx context.Context, answer *Function[struct{}, int], print *Function[string, struct{}]) error {
						panic("panic in flow")
					}),
				),
		)

		err := workflow.Do(context.Background())
		assert.ErrorContains(t, err, "panic in flow")
	})
}

func TestWorkflowErr(t *testing.T) {
	t.Run("Workflow without error, Err() should also return nil", func(t *testing.T) {
		t.Parallel()
		workflow := new(Workflow)
		workflow.Add(
			Step(Func("A", func(ctx context.Context) error { return nil })),
		)
		err := workflow.Do(context.Background())
		assert.NoError(t, err)
	})
	t.Run("Workflow with error, iterate Err() to access all errors", func(t *testing.T) {
		t.Parallel()
		workflow := new(Workflow)
		workflow.Add(
			Step(Func("A", func(ctx context.Context) error { return nil })),
			Step(Func("B", func(ctx context.Context) error { return fmt.Errorf("B") })),
		)
		err := workflow.Do(context.Background())
		assert.Error(t, err)
		for step, stepErr := range workflow.err {
			switch fmt.Sprint(step) {
			case "A":
				assert.NoError(t, stepErr)
			case "B":
				assert.ErrorContains(t, stepErr, "B")
			}
		}
	})
}

func ExampleNotify() {
	workflow := new(Workflow)
	workflow.Add(
		Step(Func("dummy step", func(ctx context.Context) error {
			fmt.Println("inside step")
			return fmt.Errorf("step error")
		})),
	).Options(
		WithNotify(Notify{
			BeforeStep: func(ctx context.Context, step Steper) context.Context {
				fmt.Printf("before step: %s\n", step)
				return ctx
			},
			AfterStep: func(ctx context.Context, step Steper, err error) {
				fmt.Printf("after step: %s error: %s\n", step, err)
			},
		}),
	)
	_ = workflow.Do(context.Background())
	// Output:
	// before step: dummy step
	// inside step
	// after step: dummy step error: step error
}

func ExampleInitDefer() {
	workflow := new(Workflow)
	workflow.Init(
		Step(Func("init", func(ctx context.Context) error {
			fmt.Println("run in init")
			return nil
		})),
	).Add(
		Step(Func("step", func(ctx context.Context) error {
			fmt.Println("run in step")
			return nil
		})),
	).Defer(
		Step(Func("defer", func(ctx context.Context) error {
			fmt.Println("run in defer")
			return nil
		})),
	)
	_ = workflow.Do(context.Background())
	// Output:
	// run in init
	// run in step
	// run in defer
}

func keys[K comparable, V any](m map[K]V) []K {
	var keys []K
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
