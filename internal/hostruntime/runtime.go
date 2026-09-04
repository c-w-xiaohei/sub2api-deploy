// Package hostruntime owns the small, per-Host durable recovery record.
package hostruntime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

const (
	stateVersion     = 1
	defaultRoot      = "/var/lib/sub2api-host"
	defaultMachineID = "/etc/machine-id"
	machineIDDomain  = "sub2api-host-machine-identity-v1"
	maxStateSize     = 1 << 20
	journalPending   = "pending"
	journalComplete  = "complete"
)

var ErrLocked = errors.New("host writer lock is held")
var stateWriteHook func(string) error
var syncDirHook func() error

type RemoteError struct {
	Category hostprotocol.ErrorCategory
	Code     hostprotocol.ErrorCode
}

func (e *RemoteError) Error() string { return string(e.Category) + ": " + string(e.Code) }

type Runtime struct {
	root            string
	machinePath     string
	expectedUID     int
	expectedRootUID int
	runner          commandRunner
	nft             nftRunner
}

func New(root, machinePath string) *Runtime {
	if root == "" {
		root = defaultRoot
	}
	if machinePath == "" {
		machinePath = defaultMachineID
	}
	return &Runtime{root: root, machinePath: machinePath, expectedUID: os.Geteuid(), expectedRootUID: os.Geteuid(), runner: execRunner{}, nft: execNFTRunner{}}
}

type State struct {
	Version         int                            `json:"version"`
	Resource        hostcontract.ResourceIdentity  `json:"resource"`
	Machine         hostcontract.MachineIdentity   `json:"machine"`
	Ownership       hostcontract.OwnershipIdentity `json:"ownership"`
	AppliedRevision string                         `json:"appliedRevision"`
	Observation     hostcontract.StableObservation `json:"observation"`
	Journal         *Journal                       `json:"journal,omitempty"`
	LastOperation   *Journal                       `json:"lastOperation,omitempty"`
	Retirement      *Retirement                    `json:"retirement,omitempty"`
}

// noCopy lets go vet flag accidental copies of the lock-owning operation.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

type Journal struct {
	Key      hostcontract.OperationKey     `json:"key"`
	Status   string                        `json:"status"`
	Approval *hostcontract.ApprovalSubject `json:"approval,omitempty"`
	Result   *hostprotocol.Result          `json:"result,omitempty"`
}
type Retirement struct {
	Machine      hostcontract.MachineIdentity   `json:"machine"`
	Ownership    hostcontract.OwnershipIdentity `json:"ownership"`
	PreserveData bool                           `json:"preserveData"`
}

// Operation is deliberately pointer-only: it owns the flock until Complete or Close.
type Operation struct {
	_       noCopy
	runtime *Runtime
	lock    *os.File
	state   State
	key     hostcontract.OperationKey
	closed  bool
}

func (o *Operation) Close() error {
	if o == nil || o.closed {
		return nil
	}
	o.closed = true
	return o.lock.Close()
}
func (o *Operation) Complete(result hostprotocol.Result, observation hostcontract.StableObservation) error {
	if o == nil || o.closed || o.state.Journal == nil || o.state.Journal.Key != o.key {
		return recovery()
	}
	if o.key.Action == hostcontract.ActionReconcile {
		if result.Status != hostprotocol.ResultApplied || result.AppliedRevision != o.key.TargetRevision || observation.Validate() != nil || !observation.Ready || observation.Machine != o.state.Machine || observation.Ownership != o.state.Ownership || observation.AppliedRevision != o.key.TargetRevision || !allObservedReady(observation) {
			_ = o.Close()
			return recovery()
		}
		o.state.AppliedRevision = observation.AppliedRevision
		o.state.Observation = observation
	} else if result.Status != hostprotocol.ResultRetired || result.Machine == nil || result.Ownership == nil || result.Retirement == nil || !result.Retirement.PreserveData || *result.Machine != o.state.Machine || *result.Ownership != o.state.Ownership {
		_ = o.Close()
		return recovery()
	} else {
		o.state.Retirement = &Retirement{Machine: o.state.Machine, Ownership: o.state.Ownership, PreserveData: true}
	}
	o.state.Journal.Status = journalComplete
	o.state.Journal.Result = &result
	err := o.runtime.writeState(o.state)
	closeErr := o.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return recovery()
	}
	return nil
}
func allObservedReady(observation hostcontract.StableObservation) bool {
	for _, app := range observation.Apps {
		if !app.Ready {
			return false
		}
	}
	for _, data := range observation.Data {
		if !data.Ready {
			return false
		}
	}
	return true
}

func (r *Runtime) MachineIdentity() (hostcontract.MachineIdentity, error) {
	b, e := os.ReadFile(r.machinePath)
	if e != nil {
		return hostcontract.MachineIdentity{}, errors.New("machine identity unavailable")
	}
	if strings.HasSuffix(string(b), "\n") {
		b = b[:len(b)-1]
	}
	id := string(b)
	if len(id) != 32 || id == strings.Repeat("0", 32) || !lowerHex(id) {
		return hostcontract.MachineIdentity{}, errors.New("machine identity invalid")
	}
	mac := hmac.New(sha256.New, []byte(machineIDDomain))
	_, _ = mac.Write([]byte(id))
	return hostcontract.MachineIdentity{Value: "mid1:" + hex.EncodeToString(mac.Sum(nil))}, nil
}
func (r *Runtime) Inspect(resource hostcontract.ResourceIdentity) (hostcontract.StableObservation, error) {
	state, e := r.readState()
	if e != nil {
		return hostcontract.StableObservation{}, remoteForRead(e)
	}
	machine, e := r.MachineIdentity()
	if e != nil || state.Resource != resource || state.Machine != machine || state.Observation.Machine != machine || state.Observation.Ownership != state.Ownership {
		return hostcontract.StableObservation{}, recovery()
	}
	return r.observe(context.Background(), state)
}

// Initialize is the narrow Create/adoption binding seam. It never guesses ownership.
func (r *Runtime) Initialize(resource hostcontract.ResourceIdentity, ownership hostcontract.OwnershipIdentity, observation hostcontract.StableObservation) error {
	lock, e := r.lock()
	if e != nil {
		return recovery()
	}
	defer lock.Close()
	machine, e := r.MachineIdentity()
	if e != nil || observation.Validate() != nil || observation.Machine != machine || observation.Ownership != ownership {
		return recovery()
	}
	if _, e = r.readState(); e == nil {
		return conflict()
	} else if !errors.Is(e, os.ErrNotExist) {
		return recovery()
	}
	return r.writeState(State{Version: stateVersion, Resource: resource, Machine: machine, Ownership: ownership, AppliedRevision: observation.AppliedRevision, Observation: observation})
}

// Bootstrap binds a fresh Host to a locally generated ownership identity, then
// reconciles the request while retaining the single writer lock.
func (r *Runtime) Bootstrap(ctx context.Context, q hostprotocol.Request) (hostprotocol.Result, error) {
	key := requestKey(q)
	if q.Action != hostcontract.ActionReconcile || q.Server.SSHAlias == "" || !utf8.ValidString(q.Server.SSHAlias) || q.Target == nil || q.Secrets == nil || q.PriorAppliedRevision == "" || q.PriorObservation != "" || key.Validate() != nil || hostcontract.ValidateTarget(*q.Target, *q.Secrets) != nil || q.Approval != nil && !q.Approval.MatchesReconcileTarget(key, *q.Target) {
		return hostprotocol.Result{}, operationFailed()
	}
	if _, err := hostcontract.ParseRevision(q.PriorAppliedRevision); err != nil {
		return hostprotocol.Result{}, operationFailed()
	}
	if validateReconcileRequest(q) != nil {
		return hostprotocol.Result{}, operationFailed()
	}
	machine, err := r.MachineIdentity()
	if err != nil {
		return hostprotocol.Result{}, recovery()
	}
	root, err := r.rootFD(false)
	rootAbsent := errors.Is(err, os.ErrNotExist)
	if err == nil {
		_ = syscall.Close(root)
	} else if !rootAbsent {
		return hostprotocol.Result{}, recovery()
	}
	if rootAbsent {
		if err := r.bootstrapDiscovery(ctx); err != nil {
			return hostprotocol.Result{}, err
		}
		if err := r.createRootExclusive(); err != nil {
			if errors.Is(err, syscall.EEXIST) {
				return hostprotocol.Result{}, conflict()
			}
			return hostprotocol.Result{}, recovery()
		}
	}
	lock, err := r.lock()
	if err != nil {
		return hostprotocol.Result{}, recovery()
	}
	var op *Operation
	admitted := false
	defer func() {
		if op == nil {
			_ = lock.Close()
			return
		}
		_ = op.Close()
	}()

	state, err := r.readState()
	if errors.Is(err, os.ErrNotExist) {
		if !rootAbsent {
			return hostprotocol.Result{}, recovery()
		}
		ownership, err := bootstrapOwnership()
		if err != nil {
			return hostprotocol.Result{}, recovery()
		}
		baseline := hostcontract.StableObservation{Machine: machine, Ownership: ownership, HostRelease: q.Target.ReleaseArtifact, AppliedRevision: q.PriorAppliedRevision, Ready: true}
		state = State{Version: stateVersion, Resource: q.Resource, Machine: machine, Ownership: ownership, AppliedRevision: q.PriorAppliedRevision, Observation: baseline, Journal: &Journal{Key: key, Status: journalPending, Approval: q.Approval}}
		if err = r.writeState(state); err != nil {
			return hostprotocol.Result{}, recovery()
		}
		op = &Operation{runtime: r, lock: lock, state: state, key: key}
	} else if err != nil {
		return hostprotocol.Result{}, recovery()
	} else {
		if state.Resource != q.Resource || state.Machine != machine || state.Retirement != nil {
			return hostprotocol.Result{}, recovery()
		}
		if state.Journal != nil && state.Journal.Key == key {
			if state.Journal.Status == journalComplete {
				if state.Journal.Result == nil {
					return hostprotocol.Result{}, recovery()
				}
				if q.Approval != nil && (state.Journal.Approval == nil || *q.Approval != *state.Journal.Approval) {
					return hostprotocol.Result{}, recovery()
				}
				return *state.Journal.Result, nil
			}
			if state.Journal.Status != journalPending || !precondition(state, key) || !validApproval(key, state, state.Journal.Approval) {
				return hostprotocol.Result{}, conflict()
			}
			if q.Approval != nil && (state.Journal.Approval == nil || *q.Approval != *state.Journal.Approval) {
				return hostprotocol.Result{}, recovery()
			}
			op = &Operation{runtime: r, lock: lock, state: state, key: key}
		} else {
			if state.Journal != nil && (state.Journal.Status != journalComplete || state.Journal.Result == nil) || state.Observation.HostRelease == q.Target.ReleaseArtifact || !precondition(state, key) || !validApproval(key, state, q.Approval) {
				return hostprotocol.Result{}, conflict()
			}
			if err = r.admitReconcile(r.persistedApproval(q)); err != nil {
				return hostprotocol.Result{}, err
			}
			admitted = true
			state.LastOperation = state.Journal
			state.Journal = &Journal{Key: key, Status: journalPending, Approval: q.Approval}
			if err = r.writeState(state); err != nil {
				return hostprotocol.Result{}, recovery()
			}
			op = &Operation{runtime: r, lock: lock, state: state, key: key}
		}
	}
	q = r.persistedApproval(q)
	if !admitted {
		if err = r.admitReconcile(q); err != nil {
			return hostprotocol.Result{}, err
		}
	}
	result, observation, err := r.reconcile(ctx, op.state, q)
	if err != nil {
		return hostprotocol.Result{}, err
	}
	if err = op.Complete(result, observation); err != nil {
		return hostprotocol.Result{}, err
	}
	return result, nil
}

func (r *Runtime) bootstrapDiscovery(ctx context.Context) error {
	for _, argv := range [][]string{
		{"container", "ls", "--all", "--filter", "label=sub2api.host", "--format", "{{.Names}}\t{{index .Labels \"sub2api.host\"}}"},
		{"network", "ls", "--filter", "label=sub2api.host", "--format", "{{.Name}}\t{{index .Labels \"sub2api.host\"}}"},
	} {
		out, err := r.runner.Run(ctx, argv, nil)
		if err != nil {
			return recovery()
		}
		if err := validateBootstrapDiscovery(out); err != nil {
			return err
		}
	}
	return nil
}

func validateBootstrapDiscovery(out []byte) error {
	if len(out) > maxCommandOutput || bytes.IndexByte(out, '\r') >= 0 {
		return recovery()
	}
	if len(out) == 0 {
		return nil
	}
	lines := strings.Split(string(out), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || len(lines) > 1024 {
		return recovery()
	}
	seen := map[string]bool{}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 2 || fields[0] == "" || !utf8.ValidString(fields[0]) || !utf8.ValidString(fields[1]) || seen[fields[0]] {
			return recovery()
		}
		seen[fields[0]] = true
		if fields[1] != "" {
			return conflict()
		}
	}
	return nil
}

func bootstrapOwnership() (hostcontract.OwnershipIdentity, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return hostcontract.OwnershipIdentity{}, err
	}
	return hostcontract.OwnershipIdentity{Value: "oid1:" + hex.EncodeToString(b)}, nil
}

func (r *Runtime) Begin(key hostcontract.OperationKey, approval *hostcontract.ApprovalSubject) (*Operation, error) {
	if key.Validate() != nil {
		return nil, recovery()
	}
	lock, e := r.lock()
	if e != nil {
		if errors.Is(e, ErrLocked) {
			return nil, ErrLocked
		}
		return nil, recovery()
	}
	fail := func(e error) (*Operation, error) { _ = lock.Close(); return nil, e }
	state, e := r.readState()
	if e != nil {
		return fail(recovery())
	}
	machine, e := r.MachineIdentity()
	if e != nil || state.Resource != key.Resource || state.Machine != machine {
		return fail(unavailable())
	}
	if state.Journal != nil {
		if state.Journal.Key == key {
			if state.Journal.Status == journalComplete {
				if approval != nil && (state.Journal.Approval == nil || *approval != *state.Journal.Approval) {
					return fail(recovery())
				}
				_ = lock.Close()
				return &Operation{state: state, key: key, closed: true}, nil
			}
			if !precondition(state, key) || !validApproval(key, state, state.Journal.Approval) {
				return fail(conflict())
			}
			if approval != nil && (state.Journal.Approval == nil || *approval != *state.Journal.Approval) {
				return fail(recovery())
			}
			return &Operation{runtime: r, lock: lock, state: state, key: key}, nil
		}
	}
	if state.LastOperation != nil && state.LastOperation.Key == key {
		if approval != nil && (state.LastOperation.Approval == nil || *approval != *state.LastOperation.Approval) {
			return fail(recovery())
		}
		_ = lock.Close()
		return &Operation{state: State{Journal: state.LastOperation}, key: key, closed: true}, nil
	}
	if state.Journal != nil && state.Journal.Status == journalPending {
		return fail(conflict())
	}
	if !precondition(state, key) {
		return fail(conflict())
	}
	if !validApproval(key, state, approval) {
		return fail(recovery())
	}
	if state.Journal != nil {
		state.LastOperation = state.Journal
	}
	state.Journal = &Journal{Key: key, Status: journalPending, Approval: approval}
	if e = r.writeState(state); e != nil {
		return fail(recovery())
	}
	return &Operation{runtime: r, lock: lock, state: state, key: key}, nil
}
func (r *Runtime) RunOperation(key hostcontract.OperationKey, approval *hostcontract.ApprovalSubject, effect func(*Operation) (hostprotocol.Result, hostcontract.StableObservation, error)) (hostprotocol.Result, error) {
	op, e := r.Begin(key, approval)
	if e != nil {
		return hostprotocol.Result{}, e
	}
	if op.closed {
		return *op.state.Journal.Result, nil
	}
	defer op.Close()
	result, observation, e := effect(op)
	if e != nil {
		return hostprotocol.Result{}, e
	}
	if e = op.Complete(result, observation); e != nil {
		return hostprotocol.Result{}, e
	}
	return result, nil
}
func precondition(s State, k hostcontract.OperationKey) bool {
	return (k.PriorAppliedRevision != "" && k.PriorAppliedRevision == s.AppliedRevision) || (k.PriorObservation != "" && k.PriorObservation == observationFingerprint(s.Observation))
}
func validApproval(k hostcontract.OperationKey, s State, a *hostcontract.ApprovalSubject) bool {
	if a == nil {
		return k.Action == hostcontract.ActionReconcile
	}
	if a.Validate() != nil || !a.Matches(k, a.AppID) {
		return false
	}
	return k.Action != hostcontract.ActionRetirePreserveData || a.Machine == s.Machine && a.Ownership == s.Ownership && a.PreserveData
}

func (r *Runtime) statePath() string { return filepath.Join(r.root, "state.json") }
func (r *Runtime) lockPath() string  { return filepath.Join(r.root, "writer.lock") }
func (r *Runtime) createRootExclusive() error {
	if filepath.Clean(r.root) != r.root || !filepath.IsAbs(r.root) {
		return errors.New("root must be absolute")
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	parts := strings.Split(strings.TrimPrefix(r.root, "/"), "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return errors.New("unsafe root")
		}
		if i == len(parts)-1 {
			if err := syscall.Mkdirat(fd, part, 0700); err != nil {
				return err
			}
			if err := syscall.Fsync(fd); err != nil {
				return err
			}
			next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			var stat syscall.Stat_t
			err = syscall.Fstat(next, &stat)
			_ = syscall.Close(next)
			if err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Mode&0077 != 0 || int(stat.Uid) != r.expectedRootUID {
				return errors.New("unsafe root")
			}
			return nil
		}
		next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		_ = syscall.Close(fd)
		fd = next
	}
	return errors.New("unsafe root")
}
func (r *Runtime) lock() (*os.File, error) {
	root, e := r.rootFD(true)
	if e != nil {
		return nil, e
	}
	defer syscall.Close(root)
	fd, e := syscall.Openat(root, "writer.lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), "writer.lock")
	var stat syscall.Stat_t
	if e := syscall.Fstat(fd, &stat); e != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&0077 != 0 || int(stat.Uid) != r.expectedUID || stat.Nlink != 1 {
		_ = f.Close()
		return nil, errors.New("unsafe lock")
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		_ = f.Close()
		if errors.Is(e, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, e
	}
	return f, nil
}

// rootFD walks from / with O_NOFOLLOW. Parents are descriptors, never path re-lookups.
func (r *Runtime) rootFD(create bool) (int, error) {
	if filepath.Clean(r.root) != r.root || !filepath.IsAbs(r.root) {
		return -1, errors.New("root must be absolute")
	}
	fd, e := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return -1, e
	}
	parts := strings.Split(strings.TrimPrefix(r.root, "/"), "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			syscall.Close(fd)
			return -1, errors.New("unsafe root")
		}
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if errors.Is(e, syscall.ENOENT) && create && i == len(parts)-1 {
			e = syscall.Mkdirat(fd, part, 0700)
			if e == nil {
				e = syscall.Fsync(fd)
			}
			if e == nil {
				next, e = syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
			}
		}
		syscall.Close(fd)
		if e != nil {
			return -1, e
		}
		fd = next
	}
	var stat syscall.Stat_t
	if e := syscall.Fstat(fd, &stat); e != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Mode&0077 != 0 || int(stat.Uid) != r.expectedRootUID {
		syscall.Close(fd)
		return -1, errors.New("unsafe root")
	}
	return fd, nil
}
func (r *Runtime) readState() (State, error) {
	root, e := r.rootFD(false)
	if e != nil {
		return State{}, e
	}
	defer syscall.Close(root)
	fd, e := syscall.Openat(root, "state.json", syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return State{}, e
	}
	var stat syscall.Stat_t
	if e = syscall.Fstat(fd, &stat); e != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&0077 != 0 || int(stat.Uid) != r.expectedUID || stat.Nlink != 1 || stat.Size > maxStateSize {
		syscall.Close(fd)
		return State{}, errors.New("unsafe state")
	}
	file := os.NewFile(uintptr(fd), "state.json") // file owns fd after this transfer.
	b, e := io.ReadAll(io.LimitReader(file, maxStateSize+1))
	closeErr := file.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil || len(b) > maxStateSize {
		return State{}, errors.New("state read")
	}
	var s State
	if strictJSON(b, &s) != nil || validateState(s) != nil {
		return State{}, errors.New("invalid state")
	}
	return s, nil
}
func (r *Runtime) writeState(s State) error {
	if validateState(s) != nil {
		return errors.New("invalid state")
	}
	root, e := r.rootFD(true)
	if e != nil {
		return e
	}
	defer syscall.Close(root)
	// The only callers hold writer.lock. A deterministic temp left behind is
	// therefore from an interrupted writer, not a concurrent operation.
	if e = r.removeStaleStateTemp(root); e != nil {
		return e
	}
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	name := ".state-tmp"
	fd, e := syscall.Openat(root, name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer syscall.Unlinkat(root, name)
	if e = writeStep("chmod"); e == nil {
		e = syscall.Fchmod(fd, 0600)
	}
	if e == nil {
		written := 0
		for written < len(b) && e == nil {
			var count int
			count, e = syscall.Write(fd, b[written:])
			if count == 0 && e == nil {
				e = io.ErrShortWrite
			}
			written += count
		}
	}
	if e == nil {
		e = writeStep("write")
	}
	if e == nil {
		e = writeStep("fsync")
		if e == nil {
			e = syscall.Fsync(fd)
		}
	}
	if e == nil {
		e = writeStep("close")
	}
	closeErr := syscall.Close(fd)
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	// The injectable dirsync failure fires before rename; a real post-rename fsync
	// failure cannot be safely rolled back without risking the newly durable state.
	if e = writeStep("dirsync"); e == nil {
		e = writeStep("rename")
	}
	if e == nil {
		e = syscall.Renameat(root, name, root, "state.json")
	}
	if e != nil {
		return e
	}
	if syncDirHook != nil {
		if e = syncDirHook(); e != nil {
			return e
		}
	}
	return syscall.Fsync(root)
}
func (r *Runtime) removeStaleStateTemp(root int) error {
	fd, err := syscall.Openat(root, ".state-tmp", syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return errors.New("unsafe state temp")
	}
	var st syscall.Stat_t
	valid := syscall.Fstat(fd, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFREG && st.Mode&0077 == 0 && int(st.Uid) == r.expectedUID && st.Nlink == 1 && st.Size <= maxStateSize
	_ = syscall.Close(fd)
	if !valid {
		return errors.New("unsafe state temp")
	}
	if err = syscall.Unlinkat(root, ".state-tmp"); err != nil {
		return err
	}
	return syscall.Fsync(root)
}
func writeStep(step string) error {
	if stateWriteHook != nil {
		return stateWriteHook(step)
	}
	return nil
}
func validateState(s State) error {
	_, revisionErr := hostcontract.ParseRevision(s.AppliedRevision)
	if s.Version != stateVersion || s.Resource.Environment == "" || s.Resource.ServerKey == "" || s.Machine.Value == "" || s.Ownership.Value == "" || revisionErr != nil || s.Observation.Validate() != nil || s.Observation.Machine != s.Machine || s.Observation.Ownership != s.Ownership || s.Observation.AppliedRevision != s.AppliedRevision {
		return errors.New("state")
	}
	if s.Journal != nil {
		j := s.Journal
		if !validJournal(*j, s, true) {
			return errors.New("journal")
		}
	}
	if s.LastOperation != nil {
		j := s.LastOperation
		if !validJournal(*j, s, false) {
			return errors.New("last operation")
		}
	}
	if s.Journal != nil && s.LastOperation != nil && s.Journal.Key == s.LastOperation.Key {
		return errors.New("journal contradiction")
	}
	if s.Retirement != nil && (s.Retirement.Machine != s.Machine || s.Retirement.Ownership != s.Ownership || !s.Retirement.PreserveData || !matchingRetire(s.Journal, s) && !matchingRetire(s.LastOperation, s)) {
		return errors.New("retirement")
	}
	return nil
}
func validJournal(j Journal, s State, current bool) bool {
	if j.Key.Validate() != nil {
		return false
	}
	if _, err := hostcontract.ParseRevision(j.Key.TargetRevision); err != nil {
		return false
	}
	if j.Key.Resource != s.Resource || !validApproval(j.Key, s, j.Approval) {
		return false
	}
	if j.Status == journalPending {
		return current && j.Result == nil && precondition(s, j.Key)
	}
	if j.Status != journalComplete || j.Result == nil {
		return false
	}
	if hostprotocolResultInvalid(*j.Result, s, j.Key) {
		return false
	}
	return !current || j.Result.Status != hostprotocol.ResultApplied || j.Result.AppliedRevision == s.AppliedRevision
}
func matchingRetire(j *Journal, s State) bool {
	return j != nil && j.Status == journalComplete && j.Key.Action == hostcontract.ActionRetirePreserveData && j.Result != nil && !hostprotocolResultInvalid(*j.Result, s, j.Key)
}
func hostprotocolResultInvalid(r hostprotocol.Result, s State, key hostcontract.OperationKey) bool {
	if r.Status == hostprotocol.ResultApplied {
		_, err := hostcontract.ParseRevision(r.AppliedRevision)
		return key.Action != hostcontract.ActionReconcile || err != nil || r.AppliedRevision != key.TargetRevision
	}
	return key.Action != hostcontract.ActionRetirePreserveData || r.Status != hostprotocol.ResultRetired || r.Machine == nil || r.Ownership == nil || r.Retirement == nil || !r.Retirement.PreserveData || *r.Machine != s.Machine || *r.Ownership != s.Ownership
}
func observationFingerprint(o hostcontract.StableObservation) string {
	b, _ := json.Marshal(o)
	sum := sha256.Sum256(append([]byte("sub2api-host-observation-v1\x00"), b...))
	return "obs1:" + hex.EncodeToString(sum[:])
}
func strictJSON(b []byte, out any) error {
	if len(b) == 0 || len(b) > maxStateSize || duplicateKey(b) {
		return errors.New("json")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	var raw any
	if e := d.Decode(&raw); e != nil || exactJSON(raw, reflect.TypeOf(out).Elem()) != nil {
		return errors.New("json")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing")
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing")
	}
	return nil
}
func exactJSON(value any, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return errors.New("type")
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = typ.Field(i).Type
			}
		}
		for name, child := range object {
			field, ok := fields[name]
			if !ok || exactJSON(child, field) != nil {
				return errors.New("field")
			}
		}
	case reflect.Slice:
		items, ok := value.([]any)
		if !ok {
			return errors.New("type")
		}
		for _, item := range items {
			if exactJSON(item, typ.Elem()) != nil {
				return errors.New("item")
			}
		}
	case reflect.Map:
		items, ok := value.(map[string]any)
		if !ok {
			return errors.New("type")
		}
		for _, item := range items {
			if exactJSON(item, typ.Elem()) != nil {
				return errors.New("item")
			}
		}
	}
	return nil
}
func duplicateKey(b []byte) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	var v func() bool
	v = func() bool {
		tok, e := d.Token()
		if e != nil {
			return true
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return false
		}
		if delim == '{' {
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				name, ok := k.(string)
				if e != nil || !ok || seen[name] {
					return true
				}
				seen[name] = true
				if v() {
					return true
				}
			}
			_, e = d.Token()
			return e != nil
		}
		if delim == '[' {
			for d.More() {
				if v() {
					return true
				}
			}
			_, e = d.Token()
			return e != nil
		}
		return true
	}
	if v() {
		return true
	}
	_, e := d.Token()
	return e != io.EOF
}
func lowerHex(v string) bool {
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func recovery() error {
	return &RemoteError{hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired}
}
func conflict() error {
	return &RemoteError{hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict}
}
func unavailable() error {
	return &RemoteError{hostprotocol.ErrorTransport, hostprotocol.CodeUnavailable}
}
func remoteForRead(e error) error {
	if errors.Is(e, os.ErrNotExist) {
		return unavailable()
	}
	return recovery()
}
