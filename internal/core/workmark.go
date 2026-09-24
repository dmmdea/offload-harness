package core

import "context"

// The working mark (0.140.6): a call's PAIR card opens "queued" when the call
// starts and turns "running" only when the lane says the work is actually
// running — for a media lane, the moment it holds the GPU. A render queued
// behind another job's lease (or a busy card) is waiting, not working, and
// its card said "Running" through the whole wait until this existed.

type workingMarkKey struct{}

// WithWorkingMark returns a context whose MarkWorking calls fn. fn must be
// safe to call more than once and from any goroutine.
func WithWorkingMark(ctx context.Context, fn func()) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, workingMarkKey{}, fn)
}

// MarkWorking tells whoever tracks this call that its real work has started.
// A context without a mark ignores it.
func MarkWorking(ctx context.Context) {
	if ctx == nil {
		return
	}
	if fn, _ := ctx.Value(workingMarkKey{}).(func()); fn != nil {
		fn()
	}
}
