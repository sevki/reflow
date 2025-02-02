// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package testutil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"sync"

	"github.com/grailbio/base/digest"
	"github.com/grailbio/reflow"
	"github.com/grailbio/reflow/errors"
	"github.com/grailbio/reflow/flow"
)

// ExecResult stores the result of a completed exec.
type ExecResult struct {
	Result  reflow.Result
	Inspect reflow.ExecInspect
}

// Exec is the Exec type used by testExecutor. They permit
// the caller to rendezvous on results.
type Exec struct {
	id      digest.Digest
	resultc chan ExecResult
	err     chan error
	config  reflow.ExecConfig
}

// newExec returns a new testExec given an ID and a config.
func newExec(id digest.Digest, config reflow.ExecConfig) *Exec {
	return &Exec{
		id:      id,
		resultc: make(chan ExecResult, 1),
		err:     make(chan error),
		config:  config,
	}
}

// ID returns the exec's ID
func (e *Exec) ID() digest.Digest { return e.id }

// URI is not implemented
func (e *Exec) URI() string { return "testexec" }

// Config returns the exec's ExecConfig.
func (e *Exec) Config() reflow.ExecConfig {
	return e.config
}

// Value rendezvous the result (value or error) of this exec.
func (e *Exec) Result(ctx context.Context) (reflow.Result, error) {
	r, err := e.result(ctx)
	return r.Result, err
}

// Promote is a no-op for the test exec.
func (e *Exec) Promote(ctx context.Context) error {
	_, err := e.result(ctx)
	return err
}

// Inspect rendezvous the result of this exec and returns the inspection output.
func (e *Exec) Inspect(ctx context.Context) (reflow.ExecInspect, error) {
	r, err := e.result(ctx)
	return r.Inspect, err
}

// Wait rendezvous this exec.
func (e *Exec) Wait(ctx context.Context) error {
	_, err := e.result(ctx)
	return err
}

// Logs is a no-op for the test exec.
func (e *Exec) Logs(ctx context.Context, stdout bool, stderr bool, follow bool) (io.ReadCloser, error) {
	_, err := e.result(ctx)
	return ioutil.NopCloser(bytes.NewBufferString("")), err
}

// Shell is not implemented
func (e *Exec) Shell(ctx context.Context) (io.ReadWriteCloser, error) {
	panic("not implemented")
}

// Ok rendezvous the value v as a successful result by this testExec.
func (e *Exec) Ok(res reflow.Result) {
	select {
	case <-e.err:
		panic("error defined")
	default:
	}

	select {
	case e.resultc <- ExecResult{Result: res}:
	default:
		panic("result already set")
	}
}

// Error rendezvous the error err as the result of this testExec.
func (e *Exec) Error(err error) {
	select {
	case <-e.resultc:
		panic("result already set")
	default:
	}
	e.err <- err
}

func (e *Exec) result(ctx context.Context) (ExecResult, error) {
	select {
	case result := <-e.resultc:
		e.resultc <- result
		result.Inspect.Config = e.config
		return result, nil
	case err := <-e.err:
		// We don't put error back--it resets the result.
		return ExecResult{}, err
	case <-ctx.Done():
		return ExecResult{}, ctx.Err()
	}
}

// Executor implements Executor for testing purposes. It allows the
// caller to await creation of Execs, to introspect execs in the
// exeutor, and to set exec results.
type Executor struct {
	reflow.Executor
	Have reflow.Resources

	Repo  reflow.Repository
	mu    sync.Mutex
	cond  *sync.Cond
	execs map[digest.Digest]*Exec
}

// Init initializes the test executor.
func (e *Executor) Init() {
	e.cond = sync.NewCond(&e.mu)
	e.execs = map[digest.Digest]*Exec{}
	e.Repo = &panicRepository{}
}

// Put defines a new exec (idempotently).
func (e *Executor) Put(ctx context.Context, id digest.Digest, config reflow.ExecConfig) (reflow.Exec, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.execs[id] == nil {
		e.execs[id] = newExec(id, config)
		e.cond.Broadcast()
	}
	return e.execs[id], nil
}

// Get retrieves an exec.
func (e *Executor) Get(ctx context.Context, id digest.Digest) (reflow.Exec, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	x := e.execs[id]
	if x == nil {
		return nil, errors.E("testutil.Executor", id, errors.NotExist)
	}
	return x, nil
}

// Remove removes the exec with id if it exists or returns an error.
func (e *Executor) Remove(ctx context.Context, id digest.Digest) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.execs[id]; !ok {
		return errors.E("testutil.Executor", id, errors.NotExist)
	}
	delete(e.execs, id)
	return nil
}

// Execs enumerates the execs managed by this executor.
func (e *Executor) Execs(ctx context.Context) ([]reflow.Exec, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var execs []reflow.Exec
	for _, e := range e.execs {
		execs = append(execs, e)
	}
	return execs, nil
}

func (*Executor) Resolve(context.Context, string) (reflow.Fileset, error) {
	panic("not implemented")
}

// Resources returns this executor's total resources.
func (e *Executor) Resources() reflow.Resources {
	return e.Have
}

// Repository returns this executor's repository.
func (e *Executor) Repository() reflow.Repository {
	return e.Repo
}

// flowId computes the exec id for this flow.
func (e *Executor) flowId(f *flow.Flow) digest.Digest {
	if f.ExecId.IsZero() {
		panic(fmt.Errorf("no exec id set for flow %v", f))
	}
	return f.ExecId
}

// Equiv tells whether this executor contains precisely a set of flows.
func (e *Executor) Equiv(flows ...*flow.Flow) bool {
	ids := map[digest.Digest]bool{}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range flows {
		ids[e.flowId(f)] = true
	}
	for id := range e.execs {
		if !ids[id] {
			return false
		}
		delete(ids, id)
	}
	return len(ids) == 0
}

// Exec rendeszvous the Exec for the provided flow.
func (e *Executor) Exec(f *flow.Flow) *Exec {
	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		if x := e.execs[e.flowId(f)]; x != nil {
			return x
		}
		e.cond.Wait()
	}
}

// Wait blocks until a Flow is defined in the executor.
func (e *Executor) Wait(f *flow.Flow) {
	e.Exec(f)
}

// Pending returns whether the flow f has a pending execution.
func (e *Executor) Pending(f *flow.Flow) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.execs[e.flowId(f)]
	return ok
}

// WaitAny returns the first of flows to be defined.
func (e *Executor) WaitAny(flows ...*flow.Flow) *flow.Flow {
	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		for _, flow := range flows {
			if e.execs[e.flowId(flow)] != nil {
				return flow
			}
		}
		e.cond.Wait()
	}
}

// Ok defines a successful result for a Flow.
func (e *Executor) Ok(f *flow.Flow, res interface{}) {
	switch arg := res.(type) {
	case reflow.Fileset:
		e.Exec(f).Ok(reflow.Result{Fileset: arg})
	case error:
		e.Exec(f).Ok(reflow.Result{Err: errors.Recover(arg)})
	default:
		panic("invalid result")
	}
}

// Error defines an erroneous result for the flow.
func (e *Executor) Error(f *flow.Flow, err error) {
	e.Exec(f).Error(err)
}

// AssignExecId assigns ExecIDs for the given set of flows using the given assertions.
func AssignExecId(a *reflow.Assertions, flows ...*flow.Flow) {
	if a == nil {
		a = new(reflow.Assertions)
	}
	for _, f := range flows {
		f.ExecId = reflow.Digester.FromDigests(f.Digest(), a.Digest())
	}
}
