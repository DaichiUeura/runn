package runn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/fatih/color"
	"github.com/goccy/go-json"
	"github.com/k1LoW/stopw"
	"github.com/rs/xid"
	"github.com/ryo-yamaoka/otchkiss"
	"go.uber.org/multierr"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

var (
	cyan   = color.New(color.FgCyan).SprintFunc()
	yellow = color.New(color.FgYellow).SprintFunc()
)

var _ otchkiss.Requester = (*operators)(nil)

type step struct {
	key           string
	runnerKey     string
	desc          string
	cond          string
	loop          *Loop
	httpRunner    *httpRunner
	httpRequest   map[string]interface{}
	dbRunner      *dbRunner
	dbQuery       map[string]interface{}
	grpcRunner    *grpcRunner
	grpcRequest   map[string]interface{}
	cdpRunner     *cdpRunner
	cdpActions    map[string]interface{}
	execRunner    *execRunner
	execCommand   map[string]interface{}
	testRunner    *testRunner
	testCond      string
	dumpRunner    *dumpRunner
	dumpRequest   *dumpRequest
	bindRunner    *bindRunner
	bindCond      map[string]string
	includeRunner *includeRunner
	includeConfig *includeConfig
	parent        *operator
	debug         bool
}

func (s *step) generateID() ID {
	id := ID{
		Type:          IDTypeStep,
		Desc:          s.desc,
		StepKey:       s.key,
		StepRunnerKey: s.runnerKey,
	}
	switch {
	case s.httpRunner != nil && s.httpRequest != nil:
		id.StepRunnerType = RunnerTypeHTTP
	case s.dbRunner != nil && s.dbQuery != nil:
		id.StepRunnerType = RunnerTypeDB
	case s.grpcRunner != nil && s.grpcRequest != nil:
		id.StepRunnerType = RunnerTypeGRPC
	case s.cdpRunner != nil && s.cdpActions != nil:
		id.StepRunnerType = RunnerTypeCDP
	case s.execRunner != nil && s.execCommand != nil:
		id.StepRunnerType = RunnerTypeExec
	case s.includeRunner != nil && s.includeConfig != nil:
		id.StepRunnerType = RunnerTypeInclude
	case s.dumpRunner != nil && s.dumpRequest != nil:
		id.StepRunnerType = RunnerTypeDump
	case s.bindRunner != nil && s.bindCond != nil:
		id.StepRunnerType = RunnerTypeBind
	case s.testRunner != nil && s.testCond != "":
		id.StepRunnerType = RunnerTypeTest
	}

	return id
}

func (s *step) ids() IDs {
	var ids IDs
	if s.parent != nil {
		ids = s.parent.ids()
	}
	ids = append(ids, s.generateID())
	return ids
}

type operator struct {
	id          string
	httpRunners map[string]*httpRunner
	dbRunners   map[string]*dbRunner
	grpcRunners map[string]*grpcRunner
	cdpRunners  map[string]*cdpRunner
	steps       []*step
	store       store
	desc        string
	useMap      bool // Use map syntax in `steps:`.
	debug       bool
	profile     bool
	interval    time.Duration
	root        string
	t           *testing.T
	thisT       *testing.T
	parent      *step
	failFast    bool
	included    bool
	cond        string
	skipTest    bool
	skipped     bool
	out         io.Writer
	bookPath    string
	beforeFuncs []func() error
	afterFuncs  []func(*RunResult) error
	sw          *stopw.Span
	capturers   capturers
	runResult   *RunResult
}

func (o *operator) Desc() string {
	return o.desc
}

func (o *operator) BookPath() string {
	return o.bookPath
}

func (o *operator) Cond() string {
	return o.cond
}

func (o *operator) Close() {
	for _, r := range o.grpcRunners {
		_ = r.Close()
	}
	for _, r := range o.cdpRunners {
		_ = r.Close()
	}
}

func (o *operator) skipStep() {
	v := map[string]interface{}{}
	v[storeStepRunKey] = false
	if o.useMap {
		o.recordAsMapped(v)
		return
	}
	o.recordAsListed(v)
}

func (o *operator) record(v map[string]interface{}) {
	if v == nil {
		v = map[string]interface{}{}
	}
	v[storeStepRunKey] = true
	if o.useMap {
		o.recordAsMapped(v)
		return
	}
	o.recordAsListed(v)
}

func (o *operator) recordAsListed(v map[string]interface{}) {
	if o.store.loopIndex != nil && *o.store.loopIndex > 0 {
		// delete values of prevous loop
		o.store.steps = o.store.steps[:len(o.store.steps)-1]
	}
	o.store.recordAsListed(v)
}

func (o *operator) recordAsMapped(v map[string]interface{}) {
	if o.store.loopIndex != nil && *o.store.loopIndex > 0 {
		// delete values of prevous loop
		delete(o.store.stepMap, o.steps[len(o.store.stepMap)-1].key)
	}
	k := o.steps[len(o.store.stepMap)].key
	o.store.recordAsMapped(k, v)
}

func (o *operator) generateID() ID {
	return ID{
		Type:        IDTypeRunbook,
		Desc:        o.desc,
		RunbookID:   o.id,
		RunbookPath: o.bookPath,
	}
}

func (o *operator) ids() IDs {
	var ids IDs
	if o.parent != nil {
		ids = o.parent.ids()
	}
	ids = append(ids, o.generateID())
	return ids
}

func New(opts ...Option) (*operator, error) {
	bk := newBook()
	if err := bk.applyOptions(opts...); err != nil {
		return nil, err
	}

	o := &operator{
		id:          generateRunbookID(),
		httpRunners: map[string]*httpRunner{},
		dbRunners:   map[string]*dbRunner{},
		grpcRunners: map[string]*grpcRunner{},
		cdpRunners:  map[string]*cdpRunner{},
		store: store{
			steps:    []map[string]interface{}{},
			stepMap:  map[string]map[string]interface{}{},
			vars:     bk.vars,
			funcs:    bk.funcs,
			bindVars: map[string]interface{}{},
			useMap:   bk.useMap,
		},
		useMap:      bk.useMap,
		desc:        bk.desc,
		debug:       bk.debug,
		profile:     bk.profile,
		interval:    bk.interval,
		t:           bk.t,
		thisT:       bk.t,
		failFast:    bk.failFast,
		included:    bk.included,
		cond:        bk.ifCond,
		skipTest:    bk.skipTest,
		out:         os.Stderr,
		bookPath:    bk.path,
		beforeFuncs: bk.beforeFuncs,
		afterFuncs:  bk.afterFuncs,
		sw:          stopw.New(),
		capturers:   bk.capturers,
		runResult:   newRunResult(bk.desc, bk.path),
	}

	if o.debug {
		o.capturers = append(o.capturers, NewDebugger(o.out))
	}

	root, err := bk.generateOperatorRoot()
	if err != nil {
		return nil, fmt.Errorf("failed to generate root (%s): %s", o.bookPath, err)
	}
	o.root = root

	for k, v := range bk.httpRunners {
		v.operator = o
		o.httpRunners[k] = v
	}
	for k, v := range bk.dbRunners {
		v.operator = o
		o.dbRunners[k] = v
	}
	for k, v := range bk.grpcRunners {
		v.operator = o
		if bk.grpcNoTLS {
			useTLS := false
			v.tls = &useTLS
		}
		o.grpcRunners[k] = v
	}
	for k, v := range bk.cdpRunners {
		v.operator = o
		o.cdpRunners[k] = v
	}

	keys := map[string]struct{}{}
	for k := range o.httpRunners {
		keys[k] = struct{}{}
	}
	for k := range o.dbRunners {
		if _, ok := keys[k]; ok {
			return nil, fmt.Errorf("duplicate runner names (%s): %s", o.bookPath, k)
		}
		keys[k] = struct{}{}
	}
	for k := range o.grpcRunners {
		if _, ok := keys[k]; ok {
			return nil, fmt.Errorf("duplicate runner names (%s): %s", o.bookPath, k)
		}
		keys[k] = struct{}{}
	}
	for k := range o.cdpRunners {
		if _, ok := keys[k]; ok {
			return nil, fmt.Errorf("duplicate runner names (%s): %s", o.bookPath, k)
		}
		keys[k] = struct{}{}
	}

	var merr error
	for k, err := range bk.runnerErrs {
		merr = multierr.Append(merr, fmt.Errorf("runner %s error: %w", k, err))
	}
	if merr != nil {
		return nil, fmt.Errorf("faild to add runners (%s): %w", o.bookPath, merr)
	}

	for i, s := range bk.rawSteps {
		key := fmt.Sprintf("%d", i)
		if o.useMap {
			key = bk.stepKeys[i]
		}
		if err := o.AppendStep(key, s); err != nil {
			return nil, fmt.Errorf("faild to append step (%s): %w", o.bookPath, err)
		}
	}

	return o, nil
}

func (o *operator) AppendStep(key string, s map[string]interface{}) error {
	if o.t != nil {
		o.t.Helper()
	}
	step := &step{key: key, parent: o, debug: o.debug}
	// if section
	if v, ok := s[ifSectionKey]; ok {
		step.cond, ok = v.(string)
		if !ok {
			return fmt.Errorf("invalid if condition: %v", v)
		}
		delete(s, ifSectionKey)
	}
	// desc section
	if v, ok := s[descSectionKey]; ok {
		step.desc, ok = v.(string)
		if !ok {
			return fmt.Errorf("invalid desc: %v", v)
		}
		delete(s, descSectionKey)
	}
	// loop section
	if v, ok := s[loopSectionKey]; ok {
		r, err := newLoop(v)
		if err != nil {
			return fmt.Errorf("invalid loop: %w\n%v", err, v)
		}
		step.loop = r
		delete(s, loopSectionKey)
	}
	// deprecated `retry:`
	if v, ok := s[deprecatedRetrySectionKey]; ok {
		r, err := newLoop(v)
		if err != nil {
			return fmt.Errorf("invalid loop: %w\n%v", err, v)
		}
		step.loop = r
		delete(s, deprecatedRetrySectionKey)
	}
	// test runner
	if v, ok := s[testRunnerKey]; ok {
		tr, err := newTestRunner(o)
		if err != nil {
			return err
		}
		step.testRunner = tr
		switch vv := v.(type) {
		case bool:
			if vv {
				step.testCond = "true"
			} else {
				step.testCond = "false"
			}
		case string:
			step.testCond = vv
		default:
			return fmt.Errorf("invalid test condition: %v", v)
		}
		delete(s, testRunnerKey)
	}
	// dump runner
	if v, ok := s[dumpRunnerKey]; ok {
		dr, err := newDumpRunner(o)
		if err != nil {
			return err
		}
		step.dumpRunner = dr
		switch vv := v.(type) {
		case string:
			step.dumpRequest = &dumpRequest{
				expr: vv,
			}
		case map[string]interface{}:
			expr, ok := vv["expr"]
			if !ok {
				return fmt.Errorf("invalid dump request: %v", vv)
			}
			out := vv["out"]
			step.dumpRequest = &dumpRequest{
				expr: expr.(string),
				out:  out.(string),
			}
		default:
			return fmt.Errorf("invalid dump request: %v", vv)
		}
		delete(s, dumpRunnerKey)
	}
	// bind runner
	if v, ok := s[bindRunnerKey]; ok {
		br, err := newBindRunner(o)
		if err != nil {
			return err
		}
		step.bindRunner = br
		vv, ok := v.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid bind condition: %v", v)
		}
		cond := map[string]string{}
		for k, vvv := range vv {
			s, ok := vvv.(string)
			if !ok {
				return fmt.Errorf("invalid bind condition: %v", v)
			}
			cond[k] = s
		}
		step.bindCond = cond
		delete(s, bindRunnerKey)
	}

	k, v, ok := pop(s)
	if ok {
		step.runnerKey = k
		switch {
		case k == includeRunnerKey:
			ir, err := newIncludeRunner(o)
			if err != nil {
				return err
			}
			step.includeRunner = ir
			c, err := parseIncludeConfig(v)
			if err != nil {
				return err
			}
			c.step = step
			step.includeConfig = c
		case k == execRunnerKey:
			er, err := newExecRunner(o)
			if err != nil {
				return err
			}
			step.execRunner = er
			vv, ok := v.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid exec command: %v", v)
			}
			step.execCommand = vv
		default:
			detected := false
			h, ok := o.httpRunners[k]
			if ok {
				step.httpRunner = h
				vv, ok := v.(map[string]interface{})
				if !ok {
					return fmt.Errorf("invalid http request: %v", v)
				}
				step.httpRequest = vv
				detected = true
			}
			db, ok := o.dbRunners[k]
			if ok && !detected {
				step.dbRunner = db
				vv, ok := v.(map[string]interface{})
				if !ok {
					return fmt.Errorf("invalid db query: %v", v)
				}
				step.dbQuery = vv
				detected = true
			}
			gc, ok := o.grpcRunners[k]
			if ok && !detected {
				step.grpcRunner = gc
				vv, ok := v.(map[string]interface{})
				if !ok {
					return fmt.Errorf("invalid gRPC request: %v", v)
				}
				step.grpcRequest = vv
				detected = true
			}
			cc, ok := o.cdpRunners[k]
			if ok && !detected {
				step.cdpRunner = cc
				vv, ok := v.(map[string]interface{})
				if !ok {
					return fmt.Errorf("invalid CDP actions: %v", v)
				}
				step.cdpActions = vv
				detected = true
			}

			if !detected {
				return fmt.Errorf("cannot find client: %s", k)
			}
		}
	}
	o.steps = append(o.steps, step)
	return nil
}

func (o *operator) Run(ctx context.Context) error {
	if o.t != nil {
		o.t.Helper()
	}
	if !o.profile {
		o.sw.Disable()
	}
	defer o.sw.Start().Stop()
	o.capturers.captureStart(o.ids(), o.bookPath, o.desc)
	defer o.capturers.captureEnd(o.ids(), o.bookPath, o.desc)
	defer o.Close()
	if err := o.run(ctx); err != nil {
		o.capturers.captureFailure(o.ids(), o.bookPath, o.desc, err)
		return err
	}
	if o.Skipped() {
		o.capturers.captureSkipped(o.ids(), o.bookPath, o.desc)
	} else {
		o.capturers.captureSuccess(o.ids(), o.bookPath, o.desc)
	}
	return nil
}

func (o *operator) DumpProfile(w io.Writer) error {
	r := o.sw.Result()
	if r == nil {
		return errors.New("no profile")
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(r); err != nil {
		return err
	}
	return nil
}

func (o *operator) Result() *RunResult {
	return o.runResult
}

func (o *operator) clearResult() {
	o.runResult = newRunResult(o.desc, o.bookPathOrID())
}

func (o *operator) run(ctx context.Context) error {
	defer o.sw.Start(o.ids().toInterfaceSlice()...).Stop()
	if o.t != nil {
		o.t.Helper()
		var err error
		o.t.Run(o.testName(), func(t *testing.T) {
			t.Helper()
			o.thisT = t
			err = o.runInternal(ctx)
			if err != nil {
				t.Error(err)
			}
		})
		o.thisT = o.t
		if err != nil {
			return fmt.Errorf("failed to run %s: %w", o.bookPathOrID(), err)
		}
		return nil
	}
	if err := o.runInternal(ctx); err != nil {
		return fmt.Errorf("failed to run %s: %w", o.bookPathOrID(), err)
	}
	return nil
}

func (o *operator) runInternal(ctx context.Context) (rerr error) {
	if o.t != nil {
		o.t.Helper()
	}
	// if
	if o.cond != "" {
		store := o.store.toMap()
		store[storeIncludedKey] = o.included
		tf, err := evalCond(o.cond, store)
		if err != nil {
			rerr = err
			return
		}
		if !tf {
			o.Debugf(yellow("Skip %s\n"), o.desc)
			o.skipped = true
			return nil
		}
	}
	// beforeFuncs
	for i, fn := range o.beforeFuncs {
		ids := append(o.ids(), ID{
			Type:      IDTypeBeforeFunc,
			FuncIndex: i,
		})
		idsi := ids.toInterfaceSlice()
		o.sw.Start(idsi...)
		if err := fn(); err != nil {
			o.sw.Stop(idsi...)
			return newBeforeFuncError(err)
		}
		o.sw.Stop(idsi...)
	}

	defer func() {
		// set run error and skipped
		o.runResult.Err = rerr
		o.runResult.Skipped = o.Skipped()

		// afterFuncs
		for i, fn := range o.afterFuncs {
			ids := append(o.ids(), ID{
				Type:      IDTypeAfterFunc,
				FuncIndex: i,
			})
			idsi := ids.toInterfaceSlice()
			o.sw.Start(idsi...)
			if aferr := fn(o.runResult); aferr != nil {
				rerr = newAfterFuncError(aferr)
			}
			o.sw.Stop(idsi...)
		}
	}()

	// steps
	for i, s := range o.steps {
		err := func() error {
			ids := s.ids()
			o.capturers.setCurrentIDs(ids)
			defer o.sw.Start(ids.toInterfaceSlice()...).Stop()
			if i != 0 {
				// interval:
				time.Sleep(o.interval)
				o.Debugln("")
			}
			if s.cond != "" {
				store := o.store.toMap()
				store[storeIncludedKey] = o.included
				tf, err := evalCond(s.cond, store)
				if err != nil {
					return err
				}
				if !tf {
					if s.desc != "" {
						o.Debugf(yellow("Skip '%s' on %s\n"), s.desc, o.stepName(i))
					} else if s.runnerKey != "" {
						o.Debugf(yellow("Skip '%s' on %s\n"), s.runnerKey, o.stepName(i))
					} else {
						o.Debugf(yellow("Skip on %s\n"), o.stepName(i))
					}
					o.skipStep()
					return nil
				}
			}
			if s.runnerKey != "" {
				o.Debugf(cyan("Run '%s' on %s\n"), s.runnerKey, o.stepName(i))
			}

			stepFn := func(t *testing.T) error {
				if t != nil {
					t.Helper()
				}
				run := false
				switch {
				case s.httpRunner != nil && s.httpRequest != nil:
					e, err := o.expand(s.httpRequest)
					if err != nil {
						return err
					}
					r, ok := e.(map[string]interface{})
					if !ok {
						return fmt.Errorf("invalid %s: %v", o.stepName(i), e)
					}
					req, err := parseHTTPRequest(r)
					if err != nil {
						return err
					}
					if err := s.httpRunner.Run(ctx, req); err != nil {
						return fmt.Errorf("http request failed on %s: %v", o.stepName(i), err)
					}
					run = true
				case s.dbRunner != nil && s.dbQuery != nil:
					e, err := o.expand(s.dbQuery)
					if err != nil {
						return err
					}
					q, ok := e.(map[string]interface{})
					if !ok {
						return fmt.Errorf("invalid %s: %v", o.stepName(i), e)
					}
					query, err := parseDBQuery(q)
					if err != nil {
						return fmt.Errorf("invalid %s: %v: %w", o.stepName(i), q, err)
					}
					if err := s.dbRunner.Run(ctx, query); err != nil {
						return fmt.Errorf("db query failed on %s: %w", o.stepName(i), err)
					}
					run = true
				case s.grpcRunner != nil && s.grpcRequest != nil:
					req, err := parseGrpcRequest(s.grpcRequest, o.expand)
					if err != nil {
						return fmt.Errorf("invalid %s: %v: %w", o.stepName(i), s.grpcRequest, err)
					}
					if err := s.grpcRunner.Run(ctx, req); err != nil {
						return fmt.Errorf("gRPC request failed on %s: %w", o.stepName(i), err)
					}
					run = true
				case s.cdpRunner != nil && s.cdpActions != nil:
					cas, err := parseCDPActions(s.cdpActions, o.expand)
					if err != nil {
						return fmt.Errorf("invalid %s: %w", o.stepName(i), err)
					}
					if err := s.cdpRunner.Run(ctx, cas); err != nil {
						return fmt.Errorf("cdp action failed on %s: %w", o.stepName(i), err)
					}
					run = true
				case s.execRunner != nil && s.execCommand != nil:
					e, err := o.expand(s.execCommand)
					if err != nil {
						return err
					}
					cmd, ok := e.(map[string]interface{})
					if !ok {
						return fmt.Errorf("invalid %s: %v", o.stepName(i), e)
					}
					command, err := parseExecCommand(cmd)
					if err != nil {
						return fmt.Errorf("invalid %s: %v", o.stepName(i), cmd)
					}
					if err := s.execRunner.Run(ctx, command); err != nil {
						return fmt.Errorf("exec command failed on %s: %v", o.stepName(i), err)
					}
					run = true
				case s.includeRunner != nil && s.includeConfig != nil:
					if err := s.includeRunner.Run(ctx, s.includeConfig); err != nil {
						return fmt.Errorf("include failed on %s: %v", o.stepName(i), err)
					}
					run = true
				}
				// dump runner
				if s.dumpRunner != nil && s.dumpRequest != nil {
					if !run {
						o.record(nil)
					}
					o.Debugf(cyan("Run '%s' on %s\n"), dumpRunnerKey, o.stepName(i))
					if err := s.dumpRunner.Run(ctx, s.dumpRequest); err != nil {
						return fmt.Errorf("dump failed on %s: %v", o.stepName(i), err)
					}
					if !run {
						run = true
					}
				}
				// bind runner
				if s.bindRunner != nil && s.bindCond != nil {
					if !run {
						o.record(nil)
					}
					o.Debugf(cyan("Run '%s' on %s\n"), bindRunnerKey, o.stepName(i))
					if err := s.bindRunner.Run(ctx, s.bindCond); err != nil {
						return fmt.Errorf("bind failed on %s: %v", o.stepName(i), err)
					}
					if !run {
						run = true
					}
				}
				// test runner
				if s.testRunner != nil && s.testCond != "" {
					if o.skipTest {
						o.Debugf(yellow("Skip '%s' on %s\n"), testRunnerKey, o.stepName(i))
						if !run {
							o.skipStep()
						}
						return nil
					}
					if !run {
						o.record(nil)
					}
					o.Debugf(cyan("Run '%s' on %s\n"), testRunnerKey, o.stepName(i))
					if err := s.testRunner.Run(ctx, s.testCond); err != nil {
						return fmt.Errorf("test failed on %s: %v", o.stepName(i), err)
					}
					if !run {
						run = true
					}
				}

				if !run {
					return fmt.Errorf("invalid runner: %v", o.stepName(i))
				}
				return nil
			}

			// loop
			if s.loop != nil {
				defer func() {
					o.store.loopIndex = nil
				}()
				retrySuccess := false
				if s.loop.Until == "" {
					retrySuccess = true
				}
				var t string
				var j int
				c, err := evalCount(s.loop.Count, o.store.toMap())
				if err != nil {
					return err
				}
				for s.loop.Loop(ctx) {
					if j >= c {
						break
					}
					jj := j
					o.store.loopIndex = &jj
					if err := stepFn(o.thisT); err != nil {
						return fmt.Errorf("loop failed: %w", err)
					}
					if s.loop.Until != "" {
						store := o.store.toMap()
						store[storePreviousKey] = o.store.previous()
						store[storeCurrentKey] = o.store.latest()
						t, err = buildTree(s.loop.Until, store)
						if err != nil {
							return fmt.Errorf("loop failed on %s: %w", o.stepName(i), err)
						}
						tf, err := evalCond(s.loop.Until, store)
						if err != nil {
							return fmt.Errorf("loop failed on %s: %w", o.stepName(i), err)
						}
						if tf {
							retrySuccess = true
							break
						}
					}
					j++
				}
				if !retrySuccess {
					err := fmt.Errorf("(%s) is not true\n%s", s.loop.Until, t)
					o.store.loopIndex = nil
					if s.loop.interval != nil {
						return fmt.Errorf("retry loop failed on %s.loop (count: %d, interval: %v): %w", o.stepName(i), c, *s.loop.interval, err)
					} else {
						return fmt.Errorf("retry loop failed on %s.loop (count: %d, minInterval: %v, maxInterval: %v): %w", o.stepName(i), c, *s.loop.minInterval, *s.loop.maxInterval, err)
					}
				}
			} else {
				if err := stepFn(o.thisT); err != nil {
					return err
				}
			}
			return nil
		}()

		if err != nil {
			rerr = err
			return
		}
	}

	return nil
}

func (o *operator) bookPathOrID() string {
	if o.bookPath != "" {
		return o.bookPath
	}
	return o.id
}

func (o *operator) testName() string {
	if o.bookPath == "" {
		return fmt.Sprintf("%s(-)", o.desc)
	}
	return fmt.Sprintf("%s(%s)", o.desc, o.bookPath)
}

func (o *operator) stepName(i int) string {
	var prefix string
	if o.store.loopIndex != nil {
		prefix = fmt.Sprintf(".loop[%d]", *o.store.loopIndex)
	}
	if o.useMap {
		return fmt.Sprintf("'%s'.steps.%s%s", o.desc, o.steps[i].key, prefix)
	}
	return fmt.Sprintf("'%s'.steps[%d]%s", o.desc, i, prefix)
}

func (o *operator) expand(in interface{}) (interface{}, error) {
	store := o.store.toMap()
	return evalExpand(in, store)
}

func (o *operator) Debugln(a string) {
	if !o.debug {
		return
	}
	_, _ = fmt.Fprintln(o.out, a)
}

func (o *operator) Debugf(format string, a ...interface{}) {
	if !o.debug {
		return
	}
	_, _ = fmt.Fprintf(o.out, format, a...)
}

func (o *operator) Warnf(format string, a ...interface{}) {
	_, _ = fmt.Fprintf(o.out, format, a...)
}

func (o *operator) Skipped() bool {
	return o.skipped
}

type operators struct {
	ops         []*operator
	t           *testing.T
	sw          *stopw.Span
	profile     bool
	shuffle     bool
	shuffleSeed int64
	shardN      int
	shardIndex  int
	sample      int
	random      int
	pmax        int64
	opts        []Option
	result      *runNResult
}

func Load(pathp string, opts ...Option) (*operators, error) {
	bk := newBook()
	opts = append([]Option{RunMatch(os.Getenv("RUNN_RUN"))}, opts...)
	if err := bk.applyOptions(opts...); err != nil {
		return nil, err
	}

	sw := stopw.New()
	ops := &operators{
		t:           bk.t,
		sw:          sw,
		profile:     bk.profile,
		shuffle:     bk.runShuffle,
		shuffleSeed: bk.runShuffleSeed,
		shardN:      bk.runShardN,
		shardIndex:  bk.runShardIndex,
		sample:      bk.runSample,
		random:      bk.runRandom,
		pmax:        1,
		opts:        opts,
	}
	if bk.runParallel {
		ops.pmax = bk.runParallelMax
	}
	books, err := Books(pathp)
	if err != nil {
		return nil, err
	}
	skipPaths := []string{}
	om := map[string]*operator{}
	for _, b := range books {
		o, err := New(append([]Option{b}, opts...)...)
		if err != nil {
			return nil, err
		}
		if bk.skipIncluded {
			for _, s := range o.steps {
				if s.includeRunner != nil && s.includeConfig != nil {
					skipPaths = append(skipPaths, filepath.Join(o.root, s.includeConfig.path))
				}
			}
		}
		om[o.bookPath] = o
	}

	for p, o := range om {
		if !bk.runMatch.MatchString(p) {
			o.Debugf(yellow("Skip %s because it does not match %s\n"), p, bk.runMatch.String())
			continue
		}
		if contains(skipPaths, p) {
			o.Debugf(yellow("Skip %s because it is already included from another runbook\n"), p)
			continue
		}
		o.sw = ops.sw
		ops.ops = append(ops.ops, o)
	}

	// Fix order of running
	sortOperators(ops.ops)
	return ops, nil
}

func (ops *operators) RunN(ctx context.Context) error {
	ops.clearResult()
	if ops.t != nil {
		ops.t.Helper()
	}
	if !ops.profile {
		ops.sw.Disable()
	}

	defer ops.sw.Start().Stop()
	defer ops.Close()
	sem := semaphore.NewWeighted(ops.pmax)
	eg, ctx := errgroup.WithContext(ctx)
	selected, err := ops.SelectedOperators()
	if err != nil {
		return err
	}
	ops.result.Total.Add(int64(len(selected)))
	for _, o := range selected {
		if err := sem.Acquire(ctx, 1); err != nil {
			return err
		}
		o := o
		eg.Go(func() error {
			defer func() {
				ops.result.RunResults.Store(o.bookPathOrID(), o.Result())
				sem.Release(1)
			}()
			o.capturers.captureStart(o.ids(), o.bookPath, o.desc)
			if err := o.run(ctx); err != nil {
				o.capturers.captureFailure(o.ids(), o.bookPath, o.desc, err)
				ops.result.Failure.Add(1)
				if o.failFast {
					o.capturers.captureEnd(o.ids(), o.bookPath, o.desc)
					return err
				}
			} else {
				if o.Skipped() {
					ops.result.Skipped.Add(1)
					o.capturers.captureSkipped(o.ids(), o.bookPath, o.desc)
				} else {
					ops.result.Success.Add(1)
					o.capturers.captureSuccess(o.ids(), o.bookPath, o.desc)
				}
			}
			o.capturers.captureEnd(o.ids(), o.bookPath, o.desc)
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	return nil
}

func (ops *operators) Operators() []*operator {
	return ops.ops
}

func (ops *operators) Close() {
	for _, o := range ops.ops {
		o.Close()
	}
}

func (ops *operators) DumpProfile(w io.Writer) error {
	r := ops.sw.Result()
	if r == nil {
		return errors.New("no profile")
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(r); err != nil {
		return err
	}
	return nil
}

func (ops *operators) Init() error {
	return nil
}

func (ops *operators) RequestOne(ctx context.Context) error {
	return ops.RunN(ctx)
}

func (ops *operators) Terminate() error {
	ops.Close()
	return nil
}

func (ops *operators) Result() *runNResult {
	return ops.result
}

func (ops *operators) clearResult() {
	for _, o := range ops.ops {
		o.clearResult()
	}
	ops.result = &runNResult{}
}

func contains(s []string, e string) bool {
	for _, v := range s {
		if e == v {
			return true
		}
	}
	return false
}

func (ops *operators) SelectedOperators() ([]*operator, error) {
	tops := make([]*operator, len(ops.ops))
	copy(tops, ops.ops)
	if ops.shuffle {
		// Shuffle order of running
		shuffleOperators(tops, ops.shuffleSeed)
	}

	if ops.shardN > 0 {
		tops = partOperators(tops, ops.shardN, ops.shardIndex)
	}
	if ops.sample > 0 {
		tops = sampleOperators(tops, ops.sample)
	}
	if ops.random > 0 {
		rops, err := randomOperators(tops, ops.opts, ops.random)
		if err != nil {
			return nil, err
		}
		for _, o := range rops {
			o.sw = ops.sw
		}
		return rops, nil
	}

	return tops, nil
}

func partOperators(ops []*operator, n, i int) []*operator {
	all := make([]*operator, len(ops))
	copy(all, ops)
	var part []*operator
	for ii, o := range all {
		if math.Mod(float64(ii), float64(n)) == float64(i) {
			part = append(part, o)
		}
	}
	return part
}

func sortOperators(ops []*operator) {
	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].bookPath == ops[j].bookPath {
			return ops[i].desc < ops[j].desc
		}
		return ops[i].bookPath < ops[j].bookPath
	})
}

func sampleOperators(ops []*operator, num int) []*operator {
	if len(ops) <= num {
		return ops
	}
	r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	var sample []*operator
	n := make([]*operator, len(ops))
	copy(n, ops)

	for i := 0; i < num; i++ {
		idx := r.Intn(len(n))
		sample = append(sample, n[idx])
		n = append(n[:idx], n[idx+1:]...)
	}
	return sample
}

func randomOperators(ops []*operator, opts []Option, num int) ([]*operator, error) {
	r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	var random []*operator
	n := make([]*operator, len(ops))
	copy(n, ops)
	for i := 0; i < num; i++ {
		idx := r.Intn(len(n))
		o, err := New(append([]Option{Book(n[idx].bookPath)}, opts...)...)
		if err != nil {
			return nil, err
		}
		random = append(random, o)
	}
	return random, nil
}

func shuffleOperators(ops []*operator, seed int64) {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec
	r.Shuffle(len(ops), func(i, j int) {
		ops[i], ops[j] = ops[j], ops[i]
	})
}

func pop(s map[string]interface{}) (string, interface{}, bool) {
	for k, v := range s {
		defer delete(s, k)
		return k, v, true
	}
	return "", nil, false
}

func generateRunbookID() string {
	return xid.New().String()
}
