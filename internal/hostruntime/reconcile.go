package hostruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

const (
	artifactInventory    = "inventory.json"
	artifactEnvPrefix    = "env-"
	artifactRoutePrefix  = "route-"
	artifactConfigPrefix = "config-"
	// inventoryVersion 3 records the Host revision independently from immutable
	// per-object shell generations.
	// It is private and unreleased, so older inventories fail closed.
	inventoryVersion = 3
	maxCommandOutput = 64 * 1024
	maxArtifactSize  = 1 << 20
)

const (
	postgresImage = "postgres:18-alpine"
	redisImage    = "redis:8-alpine"
)

var routeWriteHook func() error
var routeRestoreHook func() error
var artifactRemoveHook func(string) error
var routeRemoveHook func(string) error

type commandError struct{ ExitCode int }

func (e *commandError) Error() string { return "runtime command failed" }

type commandRunner interface {
	Run(context.Context, []string, []byte) ([]byte, error)
}
type execRunner struct{}

func (execRunner) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("invalid command")
	}
	return runProcess(ctx, "docker", argv, stdin)
}

// runProcess is intentionally package-private: production callers use only the
// fixed Docker executable above, while tests can exercise process containment.
func runProcess(ctx context.Context, executable string, argv []string, stdin []byte) ([]byte, error) {
	if executable == "" || len(argv) == 0 {
		return nil, errors.New("runtime command failed")
	}
	cmd := exec.Command(executable, argv...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := limitedBuffer{limit: make(chan struct{})}
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return nil, errors.New("runtime command failed")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	kill := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	select {
	case err := <-done:
		if err != nil || out.full.Load() {
			kill()
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return nil, &commandError{ExitCode: exit.ExitCode()}
			}
			return nil, errors.New("runtime command failed")
		}
		return out.Bytes(), nil
	case <-out.limit:
		kill()
		<-done
		return nil, errors.New("runtime command failed")
	case <-ctx.Done():
		kill()
		<-done
		return nil, ctx.Err()
	}
}

type limitedBuffer struct {
	full  atomic.Bool
	limit chan struct{}
	buf   []byte
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(b.buf)+len(p) > maxCommandOutput {
		b.full.Store(true)
		select {
		case <-b.limit:
		default:
			close(b.limit)
		}
		n := maxCommandOutput - len(b.buf)
		if n > 0 {
			b.buf = append(b.buf, p[:n]...)
		}
		return n, io.ErrShortWrite
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}
func (b *limitedBuffer) Bytes() []byte { return append([]byte(nil), b.buf...) }

type inventory struct {
	Version         int                            `json:"version"`
	Resource        hostcontract.ResourceIdentity  `json:"resource"`
	Ownership       hostcontract.OwnershipIdentity `json:"ownership"`
	AppliedRevision string                         `json:"appliedRevision"`
	Objects         []managedObject                `json:"objects"`
}
type managedObject struct {
	Role          string                          `json:"role"`
	AppToken      string                          `json:"appToken,omitempty"`
	Service       string                          `json:"service,omitempty"`
	Name          string                          `json:"name"`
	Image         string                          `json:"image,omitempty"`
	Data          []managedLink                   `json:"data,omitempty"`
	Revision      string                          `json:"revision,omitempty"`
	Active        string                          `json:"active,omitempty"`
	Env           string                          `json:"env,omitempty"`
	Type          string                          `json:"type,omitempty"`
	Port          int                             `json:"port,omitempty"`
	Persistence   bool                            `json:"persistence,omitempty"`
	DataToken     string                          `json:"dataToken,omitempty"`
	PathToken     string                          `json:"pathToken,omitempty"`
	DataIdentity  hostcontract.DataIdentity       `json:"dataIdentity,omitempty"`
	Bindings      []hostcontract.LocalDataBinding `json:"bindings,omitempty"`
	Clients       []hostcontract.LocalDataClient  `json:"clients,omitempty"`
	Config        string                          `json:"config,omitempty"`
	HBA           string                          `json:"hba,omitempty"`
	Ident         string                          `json:"ident,omitempty"`
	Hostname      string                          `json:"hostname,omitempty"`
	ReadinessPath string                          `json:"readinessPath,omitempty"`
	DrainSeconds  int                             `json:"drainSeconds,omitempty"`
}
type managedLink struct {
	Name     string                    `json:"name"`
	Identity hostcontract.DataIdentity `json:"identity"`
}
type traefikRoute struct {
	HTTP struct {
		Routers  map[string]traefikRouter  `json:"routers"`
		Services map[string]traefikService `json:"services"`
	} `json:"http"`
}
type traefikRouter struct {
	Rule        string      `json:"rule"`
	EntryPoints []string    `json:"entryPoints"`
	Service     string      `json:"service"`
	TLS         *traefikTLS `json:"tls,omitempty"`
}
type traefikTLS struct {
	CertResolver string `json:"certResolver"`
}
type traefikService struct {
	LoadBalancer struct {
		Servers []traefikServer `json:"servers"`
	} `json:"loadBalancer"`
}
type traefikServer struct {
	URL string `json:"url"`
}

func (i inventory) hasApp(token string) bool {
	for _, o := range i.Objects {
		if o.Role == "app" && o.AppToken == token {
			return true
		}
	}
	return false
}
func (i inventory) hasData(token string) bool {
	for _, o := range i.Objects {
		if o.Role == "app-data" && o.AppToken == token && len(o.Data) > 0 {
			return true
		}
	}
	return false
}
func findLocalData(i inventory, token string) managedObject {
	for _, o := range i.Objects {
		if o.Role == "local-data" && o.AppToken == token {
			return o
		}
	}
	return managedObject{}
}
func findLocalDataMetadata(i inventory, token string) managedObject {
	for _, o := range i.Objects {
		if o.Role == "local-data-meta" && o.AppToken == token {
			return o
		}
	}
	return managedObject{}
}

func (r *Runtime) Handle(ctx context.Context, q hostprotocol.Request) (hostprotocol.Result, error) {
	switch q.Action {
	case hostcontract.ActionInspect:
		s, e := r.readState()
		if e != nil {
			return hostprotocol.Result{}, remoteForRead(e)
		}
		m, e := r.MachineIdentity()
		if e != nil || s.Resource != q.Resource || s.Machine != m {
			return hostprotocol.Result{}, recovery()
		}
		if s.Retirement != nil {
			return hostprotocol.Result{Status: hostprotocol.ResultRetired, Machine: &s.Machine, Ownership: &s.Ownership, Retirement: &hostprotocol.RetirementEvidence{PreserveData: true}}, nil
		}
		observation, err := r.observe(ctx, s)
		if err != nil {
			return hostprotocol.Result{}, err
		}
		result := hostprotocol.Result{Status: hostprotocol.ResultInspected, Observation: &observation}
		if s.Journal != nil && s.Journal.Key.Action == hostcontract.ActionReconcile && s.Journal.Key.TargetRevision == q.TargetRevision && (s.Journal.Status == journalPending || s.Journal.Status == journalComplete) {
			evidence := &hostprotocol.OperationEvidence{Key: s.Journal.Key, Status: hostprotocol.OperationStatus(s.Journal.Status)}
			if s.Journal.Approval != nil {
				approval := *s.Journal.Approval
				evidence.Approval = &approval
			}
			result.OperationEvidence = evidence
		}
		return result, nil
	case hostcontract.ActionReconcile:
		return r.Reconcile(ctx, q)
	case hostcontract.ActionRetirePreserveData:
		return r.Retire(ctx, q)
	}
	return hostprotocol.Result{}, operationFailed()
}
func requestKey(q hostprotocol.Request) hostcontract.OperationKey {
	return hostcontract.OperationKey{Resource: q.Resource, Action: q.Action, TargetRevision: q.TargetRevision, PriorAppliedRevision: q.PriorAppliedRevision, PriorObservation: q.PriorObservation}
}

func (r *Runtime) Reconcile(ctx context.Context, q hostprotocol.Request) (hostprotocol.Result, error) {
	if validateReconcileRequest(q) != nil {
		return hostprotocol.Result{}, operationFailed()
	}
	if result, ok := r.reconcileTerminalResult(requestKey(q), q); ok {
		return result, nil
	}
	if e := r.admitReconcile(r.persistedApproval(q)); e != nil {
		return hostprotocol.Result{}, e
	}
	key := requestKey(q)
	return r.RunOperation(key, q.Approval, func(op *Operation) (hostprotocol.Result, hostcontract.StableObservation, error) {
		return r.reconcile(ctx, op.state, r.persistedApproval(q))
	})
}
func (r *Runtime) persistedApproval(q hostprotocol.Request) hostprotocol.Request {
	if q.Approval != nil {
		return q
	}
	state, err := r.readState()
	if err == nil && state.Journal != nil && state.Journal.Status == journalPending && state.Journal.Key == requestKey(q) && state.Journal.Approval != nil {
		approval := *state.Journal.Approval
		q.Approval = &approval
	}
	return q
}
func (r *Runtime) reconcileTerminalResult(key hostcontract.OperationKey, q hostprotocol.Request) (hostprotocol.Result, bool) {
	if q.Target == nil || q.Secrets == nil || hostcontract.ValidateTarget(*q.Target, *q.Secrets) != nil {
		return hostprotocol.Result{}, false
	}
	s, err := r.readState()
	if err != nil || r.validateLiveState(s, key.Resource) != nil {
		return hostprotocol.Result{}, false
	}
	if s.Journal != nil && s.Journal.Key == key && q.Approval != nil && (s.Journal.Approval == nil || *q.Approval != *s.Journal.Approval) {
		return hostprotocol.Result{}, false
	}
	return r.terminalResult(key)
}

func (r *Runtime) terminalResult(key hostcontract.OperationKey) (hostprotocol.Result, bool) {
	state, err := r.readState()
	if err != nil || state.Journal == nil || state.Journal.Key != key || state.Journal.Status != journalComplete || state.Journal.Result == nil {
		return hostprotocol.Result{}, false
	}
	return *state.Journal.Result, true
}
func (r *Runtime) admitReconcile(q hostprotocol.Request) error {
	if validateReconcileRequest(q) != nil {
		return operationFailed()
	}
	state, e := r.readState()
	if e != nil || r.validateLiveState(state, q.Resource) != nil {
		return recovery()
	}
	inv, e := r.readInventory()
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return recovery()
	}
	if e == nil {
		if validateInventoryForState(inv, state) != nil {
			return recovery()
		}
		if e = r.validatePersistedRoutes(inv, q); e != nil {
			return e
		}
		if e = r.requireApproval(inv, q); e != nil {
			return e
		}
		if e = r.checkKnownOwnedForRequest(context.Background(), state, inv, q, true); e != nil {
			return e
		}
		if e = r.admitPostgresPasswordChange(context.Background(), state, inv, q); e != nil {
			return e
		}
	} else {
		inv = inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, AppliedRevision: state.AppliedRevision}
	}
	if e = r.preflightTargets(context.Background(), state, inv, q); e != nil {
		return e
	}
	if q.Target.ReverseProxy != nil && r.validateACMEPath() != nil {
		return recovery()
	}
	if e = r.admitNetwork(context.Background(), state); e != nil {
		return e
	}
	return nil
}

// validateReconcileRequest is deliberately state-free so Bootstrap can reject
// deterministic bad input before inspecting the host or creating its root.
func validateReconcileRequest(q hostprotocol.Request) error {
	if q.Action != hostcontract.ActionReconcile || q.Target == nil || q.Secrets == nil || hostcontract.ValidateTarget(*q.Target, *q.Secrets) != nil || q.Target.MicroSocks != nil || len(q.Target.Connectors) > 0 {
		return errors.New("request")
	}
	for _, a := range q.Target.Apps {
		secret := (*q.Secrets).Apps[a.ID]
		if !safeEnvironment(a, secret) || !validAppDataLinks(a, secret) || !validHostname(a.Hostname) || appDrainSeconds(a) == 0 || len(envBytes(a, secret)) > maxArtifactSize {
			return errors.New("app")
		}
	}
	for _, target := range q.Target.DataServices {
		secret, ok := (*q.Secrets).LocalDataServices[target.ID]
		if !validClientAppID(target.ID) || !ok || !validLocalPassword(secret.AdminPassword) || !validLocalDataCredentials(target, secret) {
			return errors.New("local data")
		}
		if target.Type == "postgres" {
			if len(secret.AdminPassword)+1 > maxArtifactSize {
				return errors.New("postgres")
			}
			var hba strings.Builder
			hba.WriteString("local all all peer map=s2h_admin\n")
			for _, client := range target.Clients {
				hba.WriteString("host " + client.Database + " " + client.Username + " all scram-sha-256\n")
			}
			hba.WriteString("local all all reject\nhost all all all reject\n")
			if hba.Len() > maxArtifactSize {
				return errors.New("postgres")
			}
		}
		if target.Type == "redis" {
			config, err := redisConfig(target, secret)
			if err != nil || len(config) > maxArtifactSize || len("REDISCLI_AUTH="+secret.AdminPassword+"\n") > maxArtifactSize {
				return errors.New("redis")
			}
		}
	}
	if q.Target.ReverseProxy != nil && (q.Secrets.ReverseProxy == nil || unsafeInline(q.Target.ReverseProxy.ACMEEmail) || unsafeInline((*q.Secrets).ReverseProxy.DNSChallengeToken) || len(traefikStaticConfig(q.Target.ReverseProxy.ACMEEmail)) > maxArtifactSize || len("CF_DNS_API_TOKEN="+(*q.Secrets).ReverseProxy.DNSChallengeToken+"\nACME_EMAIL="+q.Target.ReverseProxy.ACMEEmail+"\n") > maxArtifactSize) {
		return errors.New("proxy")
	}
	return nil
}
func (r *Runtime) admitPostgresPasswordChange(ctx context.Context, state State, inv inventory, q hostprotocol.Request) error {
	pending := state.Journal != nil && state.Journal.Status == journalPending && state.Journal.Key == requestKey(q)
	for _, target := range q.Target.DataServices {
		if target.Type != "postgres" {
			continue
		}
		old := findLocalData(inv, localDataToken(target.ID))
		if old.Name == "" {
			continue
		}
		changed, err := r.postgresPasswordChanged(old, (*q.Secrets).LocalDataServices[target.ID].AdminPassword)
		if err != nil {
			return recovery()
		}
		if !changed {
			continue
		}
		proposed, proposedOK := pendingReplacement(state, old, q)
		present, err := r.ownedPresentEither(ctx, inv, old, proposed, pending && proposedOK)
		if err != nil {
			return err
		}
		if !present && !pending {
			return recovery()
		}
	}
	return nil
}
func (r *Runtime) validateLiveState(s State, resource hostcontract.ResourceIdentity) error {
	machine, err := r.MachineIdentity()
	if err != nil || s.Resource != resource || s.Machine != machine {
		return recovery()
	}
	return nil
}
func validateInventoryForState(inv inventory, state State) error {
	if validateInventory(inv) != nil || inv.Resource != state.Resource || inv.Ownership != state.Ownership {
		return errors.New("inventory state")
	}
	return nil
}
func (r *Runtime) preflightTargets(ctx context.Context, state State, inv inventory, q hostprotocol.Request) error {
	pending := state.Journal != nil && state.Journal.Status == journalPending && state.Journal.Key == requestKey(q)
	for _, target := range q.Target.DataServices {
		token := localDataToken(target.ID)
		old := findLocalData(inv, token)
		if old.Name == "" {
			old = findLocalDataMetadata(inv, token)
		}
		if old.Type != "" && old.Type != target.Type {
			return recovery()
		}
		candidate := localObject(state, target, q.TargetRevision)
		if old.Type != "" {
			candidate.DataToken, candidate.PathToken = old.DataToken, old.PathToken
			if localShellEqual(old, candidate, target) {
				candidate = old
			}
		}
		if old.Name == "" {
			exists, err := r.candidateExists(ctx, inv, candidate)
			if err != nil {
				return err
			}
			if exists && !pending {
				return conflict()
			}
		}
	}
	if q.Target.ReverseProxy != nil {
		candidate := proxyObject(state, *q.Target.ReverseProxy, q.TargetRevision)
		if findProxy(inv).Name == "" {
			exists, err := r.candidateExists(ctx, inv, candidate)
			if err != nil {
				return err
			}
			if exists && !pending {
				return conflict()
			}
		}
	}
	for _, target := range q.Target.Apps {
		token := appToken(target.ID)
		old := findApp(inv, token)
		if old.Name == "" {
			if _, err := r.readArtifactBytes(routeName(token)); err == nil {
				candidate := appObject(state, target, q.TargetRevision, "green")
				if state.Journal == nil || state.Journal.Status != journalPending || state.Journal.Key != requestKey(q) || !r.routeMatches(inv, candidate) {
					return conflict()
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return recovery()
			}
		}
		active := old.Active
		if active == "" {
			active = "blue"
		}
		inactive := "blue"
		if active == "blue" {
			inactive = "green"
		}
		candidate := appObject(state, target, q.TargetRevision, inactive)
		if err := r.inspectCandidate(ctx, inv, candidate); err != nil {
			return err
		}
	}
	return nil
}
func (r *Runtime) validatePersistedRoutes(inv inventory, q hostprotocol.Request) error {
	state, err := r.readState()
	if err != nil {
		return recovery()
	}
	for _, current := range inv.Objects {
		if current.Role != "app" || r.routeMatches(inv, current) {
			continue
		}
		if state.Journal == nil || state.Journal.Status != journalPending || state.Journal.Key != requestKey(q) {
			return recovery()
		}
		if _, err := r.readArtifactBytes(routeName(current.AppToken)); errors.Is(err, os.ErrNotExist) {
			if _, present := findTarget(q.Target.Apps, current.AppToken); !present {
				continue
			}
		}
		for _, target := range q.Target.Apps {
			if appToken(target.ID) != current.AppToken {
				continue
			}
			inactive := "blue"
			if current.Active == "blue" {
				inactive = "green"
			}
			candidate := appObject(state, target, q.TargetRevision, inactive)
			if r.routeMatches(inv, candidate) {
				goto next
			}
		}
		return recovery()
	next:
	}
	return nil
}
func findTarget(apps []hostcontract.AppTarget, token string) (hostcontract.AppTarget, bool) {
	for _, app := range apps {
		if appToken(app.ID) == token {
			return app, true
		}
	}
	return hostcontract.AppTarget{}, false
}
func (r *Runtime) requireApproval(inv inventory, q hostprotocol.Request) error {
	type change struct {
		app      string
		old, new hostcontract.DataIdentity
	}
	var changed []change
	for _, a := range q.Target.Apps {
		token := appToken(a.ID)
		old := managedLinks(inv, token)
		if len(old) == 0 {
			continue
		}
		appChanged, ambiguous := matchLinks(old, links(a))
		if ambiguous {
			return approvalRequired()
		}
		for _, linkChange := range appChanged {
			changed = append(changed, change{app: a.ID, old: linkChange.old, new: linkChange.new})
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if len(changed) != 1 || q.Approval == nil {
		return approvalRequired()
	}
	c := changed[0]
	a := q.Approval
	if a.Kind != hostcontract.ApprovalDataLink || a.AppID != c.app || a.OldData != c.old || a.NewData != c.new || a.TargetRevision != q.TargetRevision || a.Resource != q.Resource {
		return approvalRequired()
	}
	return nil
}

type linkChange struct{ old, new hostcontract.DataIdentity }

func matchLinks(old, new []managedLink) ([]linkChange, bool) {
	oldByName, newByName := map[string]hostcontract.DataIdentity{}, map[string]hostcontract.DataIdentity{}
	for _, link := range old {
		oldByName[link.Name] = link.Identity
	}
	for _, link := range new {
		newByName[link.Name] = link.Identity
	}
	var changed []linkChange
	for _, name := range sortedLinkNames(oldByName) {
		oldIdentity := oldByName[name]
		if newIdentity, ok := newByName[name]; ok {
			delete(oldByName, name)
			delete(newByName, name)
			if oldIdentity != newIdentity {
				changed = append(changed, linkChange{old: oldIdentity, new: newIdentity})
			}
		}
	}
	for _, oldName := range sortedLinkNames(oldByName) {
		oldIdentity := oldByName[oldName]
		for _, newName := range sortedLinkNames(newByName) {
			if oldIdentity == newByName[newName] {
				delete(oldByName, oldName)
				delete(newByName, newName)
				break
			}
		}
	}
	oldByKind, newByKind := map[string][]hostcontract.DataIdentity{}, map[string][]hostcontract.DataIdentity{}
	for _, identity := range oldByName {
		oldByKind[identity.Kind] = append(oldByKind[identity.Kind], identity)
	}
	for _, identity := range newByName {
		newByKind[identity.Kind] = append(newByKind[identity.Kind], identity)
	}
	kinds := make(map[string]bool, len(oldByKind)+len(newByKind))
	for kind := range oldByKind {
		kinds[kind] = true
	}
	for kind := range newByKind {
		kinds[kind] = true
	}
	for _, kind := range sortedLinkNames(kinds) {
		oldIdentities, newIdentities := oldByKind[kind], newByKind[kind]
		if len(oldIdentities) == 1 && len(newIdentities) == 1 {
			changed = append(changed, linkChange{old: oldIdentities[0], new: newIdentities[0]})
			continue
		}
		if len(oldIdentities) > 0 && len(newIdentities) > 0 {
			return nil, true
		}
	}
	return changed, false
}
func sortedLinkNames[V any](links map[string]V) []string {
	names := make([]string, 0, len(links))
	for name := range links {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func managedLinks(inv inventory, token string) []managedLink {
	for _, o := range inv.Objects {
		if (o.Role == "app" || o.Role == "app-data") && o.AppToken == token {
			return o.Data
		}
	}
	return nil
}
func (r *Runtime) checkKnownOwned(ctx context.Context, inv inventory, allowAbsent bool) error {
	for _, o := range inv.Objects {
		if o.Name == "" {
			continue
		}
		present, e := r.ownedPresent(ctx, inv, o)
		if e != nil || (!present && !allowAbsent) {
			if e == nil {
				return recovery()
			}
			return e
		}
	}
	return nil
}
func (r *Runtime) checkKnownOwnedForRequest(ctx context.Context, state State, inv inventory, q hostprotocol.Request, allowAbsent bool) error {
	pending := state.Journal != nil && state.Journal.Status == journalPending && state.Journal.Key == requestKey(q)
	for _, old := range inv.Objects {
		if old.Name == "" {
			continue
		}
		proposed, ok := pendingReplacement(state, old, q)
		present, err := r.ownedPresentEither(ctx, inv, old, proposed, pending && ok)
		if err != nil || (!present && !allowAbsent) {
			if err == nil {
				return recovery()
			}
			return err
		}
	}
	return nil
}

// observe is intentionally read-only: it only lists known containers/networks
// and performs the fixed readiness probes needed to report live drift.
func (r *Runtime) observe(ctx context.Context, state State) (hostcontract.StableObservation, error) {
	inv, err := r.readInventory()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			observation := state.Observation
			observation.Drifted, observation.Ready = true, false
			return observation, nil
		}
		return hostcontract.StableObservation{}, recovery()
	}
	if validateInventoryForState(inv, state) != nil {
		return hostcontract.StableObservation{}, recovery()
	}
	if err = r.admitNetwork(ctx, state); err != nil {
		return hostcontract.StableObservation{}, recovery()
	}
	network, err := r.runner.Run(ctx, networkListArgs(state), nil)
	if err != nil {
		return hostcontract.StableObservation{}, recovery()
	}
	observation := state.Observation
	observation.Drifted, observation.Ready = false, true
	apps := map[string]bool{}
	data := map[hostcontract.DataIdentity]bool{}
	inventoryApps := map[string]bool{}
	inventoryData := map[hostcontract.DataIdentity]bool{}
	stableApps := map[string]hostcontract.AppObservation{}
	stableData := map[hostcontract.DataIdentity]bool{}
	for _, app := range observation.Apps {
		stableApps[appToken(app.ID)] = app
	}
	for _, datum := range observation.Data {
		stableData[datum.Identity] = true
	}
	if strings.TrimSpace(string(network)) == "" {
		observation.Drifted, observation.Ready = true, false
	}
	for _, object := range inv.Objects {
		if object.Name == "" {
			continue
		}
		present, observed := r.ownedPresent(ctx, inv, object)
		if observed != nil {
			return hostcontract.StableObservation{}, recovery()
		}
		if !present {
			observation.Drifted, observation.Ready = true, false
			continue
		}
		if object.Role != "local-data" && object.Revision != state.AppliedRevision {
			observation.Drifted, observation.Ready = true, false
		}
		switch object.Role {
		case "app":
			inventoryApps[object.AppToken] = true
			stable, known := stableApps[object.AppToken]
			if !known || stable.ActiveImage != object.Image {
				observation.Drifted, observation.Ready = true, false
			}
			route, routeErr := r.readArtifactBytes(routeName(object.AppToken))
			if routeErr != nil && !errors.Is(routeErr, os.ErrNotExist) {
				return hostcontract.StableObservation{}, recovery()
			}
			if routeErr == nil {
				var parsed traefikRoute
				if strictJSON(route, &parsed) != nil || !validRouteDocument(parsed) {
					return hostcontract.StableObservation{}, recovery()
				}
			}
			if routeErr != nil || !r.routeMatches(inv, object) || r.ready(ctx, object.Name, object.ReadinessPath) != nil {
				observation.Drifted, observation.Ready = true, false
			} else {
				apps[object.AppToken] = true
			}
		case "local-data":
			if !r.localSecurityArtifactsValid(object) {
				observation.Drifted, observation.Ready = true, false
			}
			if object.Type == "postgres" {
				expected, expectedErr := postgresCatalogExpected(state, inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership}, hostcontract.LocalDataServiceTarget{ID: object.Service, Type: object.Type, Clients: object.Clients}, state.AppliedRevision)
				if expectedErr != nil {
					return hostcontract.StableObservation{}, recovery()
				}
				catalog, catalogErr := r.postgresClassification(ctx, state, object, expected)
				if catalogErr != nil || catalog.State == postgresCatalogProtocolForeign || catalog.State == postgresCatalogProtocolUnavailable {
					return hostcontract.StableObservation{}, recovery()
				}
				if catalog.State != postgresCatalogProtocolExact {
					observation.Drifted, observation.Ready = true, false
				}
			}
			if r.exactLocalPublications(ctx, object) != nil {
				observation.Drifted, observation.Ready = true, false
			}
			inventoryData[object.DataIdentity] = true
			if !stableData[object.DataIdentity] {
				observation.Drifted, observation.Ready = true, false
			}
			if r.localReady(ctx, object) != nil {
				observation.Drifted, observation.Ready = true, false
			} else {
				data[object.DataIdentity] = true
			}
		case "proxy":
			if r.proxyReady(ctx, object) != nil {
				observation.Drifted, observation.Ready = true, false
			}
		}
	}
	if err := r.observeNft(ctx, state, inv); err != nil {
		observation.Drifted, observation.Ready = true, false
	}
	for index := range observation.Apps {
		token := appToken(observation.Apps[index].ID)
		observation.Apps[index].Ready = apps[token]
		if !observation.Apps[index].Ready {
			observation.Ready = false
		}
	}
	for index := range observation.Data {
		observation.Data[index].Ready = data[observation.Data[index].Identity]
		if !observation.Data[index].Ready {
			observation.Ready = false
		}
	}
	if len(inventoryApps) != len(observation.Apps) || len(inventoryData) != len(observation.Data) || len(apps) != len(observation.Apps) || len(data) != len(observation.Data) {
		observation.Drifted, observation.Ready = true, false
	}
	if state.Journal != nil && state.Journal.Status == journalPending {
		observation.Drifted, observation.Ready = true, false
	}
	if observation.Ready {
		observation.Drifted = false
	}
	return observation, nil
}
func validRouteDocument(route traefikRoute) bool {
	if len(route.HTTP.Routers) == 0 || len(route.HTTP.Services) == 0 {
		return false
	}
	for _, router := range route.HTTP.Routers {
		if router.Rule == "" || len(router.EntryPoints) == 0 || router.Service == "" {
			return false
		}
		if _, ok := route.HTTP.Services[router.Service]; !ok {
			return false
		}
	}
	for _, service := range route.HTTP.Services {
		if len(service.LoadBalancer.Servers) == 0 {
			return false
		}
		for _, server := range service.LoadBalancer.Servers {
			if server.URL == "" {
				return false
			}
		}
	}
	return true
}
func pendingReplacement(state State, old managedObject, q hostprotocol.Request) (managedObject, bool) {
	if old.Role == "local-data" {
		for _, target := range q.Target.DataServices {
			if localDataToken(target.ID) == old.AppToken {
				candidate := localObject(state, target, q.TargetRevision)
				candidate.DataToken, candidate.PathToken = old.DataToken, old.PathToken
				if localShellEqual(old, candidate, target) {
					candidate = old
				}
				return candidate, true
			}
		}
	}
	if old.Role == "proxy" && q.Target.ReverseProxy != nil {
		return proxyObject(state, *q.Target.ReverseProxy, q.TargetRevision), true
	}
	return managedObject{}, false
}
func (r *Runtime) ownedPresent(ctx context.Context, inv inventory, o managedObject) (bool, error) {
	return r.ownedPresentEither(ctx, inv, o, managedObject{}, false)
}
func (r *Runtime) ownedPresentEither(ctx context.Context, inv inventory, o, alternate managedObject, allowAlternate bool) (bool, error) {
	out, e := r.runner.Run(ctx, []string{"container", "ls", "--all", "--filter", "name=^/" + o.Name + "$", "--format", "{{.Names}}\t{{index .Labels \"sub2api.host\"}}\t{{index .Labels \"sub2api.host.target\"}}"}, nil)
	if e != nil {
		return false, recovery()
	}
	rows := strings.FieldsFunc(string(out), func(r rune) bool { return r == '\n' || r == '\r' })
	if len(rows) == 0 {
		return false, nil
	}
	if len(rows) != 1 {
		return false, recovery()
	}
	fields := strings.Split(rows[0], "\t")
	if len(fields) != 3 || fields[0] != o.Name {
		return false, recovery()
	}
	if fields[1] != ownershipLabelFor(inv.Resource, inv.Ownership, o.Role, o.AppToken, o.Active) {
		return false, conflict()
	}
	if fields[2] != targetLabelFor(o) && (!allowAlternate || fields[2] != targetLabelFor(alternate)) {
		return false, conflict()
	}
	return true, nil
}
func (r *Runtime) inspectOwned(ctx context.Context, inv inventory, o managedObject) error {
	present, e := r.ownedPresent(ctx, inv, o)
	if e != nil {
		return e
	}
	if !present {
		return recovery()
	}
	return nil
}
func (r *Runtime) reconcile(ctx context.Context, s State, q hostprotocol.Request) (hostprotocol.Result, hostcontract.StableObservation, error) {
	inv, e := r.readInventory()
	if errors.Is(e, os.ErrNotExist) {
		inv = inventory{Version: inventoryVersion, Resource: s.Resource, Ownership: s.Ownership, AppliedRevision: s.AppliedRevision}
	} else if e != nil {
		return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
	}
	if validateInventoryForState(inv, s) != nil {
		return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
	}
	if e = r.checkKnownOwnedForRequest(ctx, s, inv, q, true); e != nil {
		return hostprotocol.Result{}, hostcontract.StableObservation{}, e
	}
	if err := r.ensureNetwork(ctx, s); err != nil {
		return hostprotocol.Result{}, hostcontract.StableObservation{}, err
	}
	if err := r.reconcileNft(ctx, s, nftUnion(inv, q.Target.DataServices)); err != nil {
		return hostprotocol.Result{}, hostcontract.StableObservation{}, err
	}
	kept := make([]managedObject, 0, len(inv.Objects))
	data, proxy, err := r.reconcileLocal(ctx, s, inv, q)
	if err != nil {
		return hostprotocol.Result{}, hostcontract.StableObservation{}, err
	}
	kept = append(kept, data...)
	if proxy.Name != "" {
		kept = append(kept, proxy)
	}
	if err := r.reconcileNft(ctx, s, q.Target.DataServices); err != nil {
		return hostprotocol.Result{}, hostcontract.StableObservation{}, err
	}
	target := map[string]hostcontract.AppTarget{}
	for _, a := range q.Target.Apps {
		target[appToken(a.ID)] = a
	}
	for _, o := range inv.Objects {
		if o.Role != "app" {
			if o.Role == "local-data" || o.Role == "local-data-meta" || o.Role == "proxy" {
				continue
			}
			if o.Role == "app-data" && target[o.AppToken].ID != "" {
				continue
			}
			kept = append(kept, o)
			continue
		}
		if target[o.AppToken].ID != "" {
			continue
		}
		if e = r.removeRouteProgress(inv, o, s.Journal != nil && s.Journal.Status == journalPending && s.Journal.Key == requestKey(q)); e != nil {
			return hostprotocol.Result{}, hostcontract.StableObservation{}, e
		}
		if e = r.removeOwnedProgress(ctx, inv, o, s.Journal != nil && s.Journal.Status == journalPending && s.Journal.Key == requestKey(q)); e != nil {
			return hostprotocol.Result{}, hostcontract.StableObservation{}, e
		}
		if o.Env != "" {
			if e = r.removeEnv(o.Env); e != nil {
				return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
			}
		}
		if len(o.Data) != 0 {
			kept = append(kept, managedObject{Role: "app-data", AppToken: o.AppToken, Data: o.Data})
		}
	}
	for _, a := range q.Target.Apps {
		token := appToken(a.ID)
		old := findApp(inv, token)
		if old.Name != "" && old.Image == a.Image && old.Revision == q.TargetRevision && r.routeMatches(inv, old) && r.inspectOwned(ctx, inv, old) == nil && r.ready(ctx, old.Name, a.ReadinessPath) == nil {
			kept = append(kept, old)
			continue
		}
		active := "blue"
		for _, o := range inv.Objects {
			if o.Role == "app" && o.AppToken == token {
				active = o.Active
			}
		}
		inactive := "green"
		if active == "green" {
			inactive = "blue"
		}
		candidate := appObject(s, a, q.TargetRevision, inactive)
		if e = r.inspectCandidate(ctx, inv, candidate); e != nil {
			return hostprotocol.Result{}, hostcontract.StableObservation{}, e
		}
		exists, err := r.candidateExists(ctx, inv, candidate)
		if err != nil {
			return hostprotocol.Result{}, hostcontract.StableObservation{}, err
		}
		if e = r.writeArtifact(candidate.Env, envBytes(a, (*q.Secrets).Apps[a.ID]), 0600); e != nil {
			return hostprotocol.Result{}, hostcontract.StableObservation{}, operationFailed()
		}
		dataToken := appDataToken(token)
		if e = r.ensureDataDir(dataToken); e != nil {
			return hostprotocol.Result{}, hostcontract.StableObservation{}, operationFailed()
		}
		if !exists {
			if e = r.docker(ctx, "run", "-d", "--restart", "unless-stopped", "--label", "sub2api.host="+ownershipLabelFor(s.Resource, s.Ownership, "app", token, inactive), "--label", "sub2api.host.target="+targetLabelFor(candidate), "--name", candidate.Name, "--network", networkName(s), "--network-alias", a.ID, "--env-file", r.artifactPath(candidate.Env), "-v", r.dataPath(dataToken)+":/app/data", a.Image); e != nil {
				if exists, observed := r.candidateExists(ctx, inv, candidate); observed != nil {
					return hostprotocol.Result{}, hostcontract.StableObservation{}, observed
				} else if !exists {
					return hostprotocol.Result{}, hostcontract.StableObservation{}, operationFailed()
				}
				return hostprotocol.Result{}, hostcontract.StableObservation{}, operationFailed()
			}
		}
		if e = r.ready(ctx, candidate.Name, a.ReadinessPath); e != nil {
			if e = r.removeOwned(ctx, inv, candidate); e != nil {
				return hostprotocol.Result{}, hostcontract.StableObservation{}, e
			}
			if e = r.removeEnv(candidate.Env); e != nil {
				return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
			}
			return hostprotocol.Result{}, hostcontract.StableObservation{}, operationFailed()
		}
		oldRoute, routeExisted := r.routeBytes(token)
		if !r.routeMatches(inv, candidate) {
			if e = r.writeRoute(inv, candidate); e != nil {
				return hostprotocol.Result{}, hostcontract.StableObservation{}, operationFailed()
			}
		}
		if e = r.postRouteReady(ctx, proxy, candidate, a.ReadinessPath); e != nil {
			var restoreErr error
			restoreErr = r.restoreRoute(token, oldRoute, routeExisted)
			if restoreErr != nil {
				return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
			}
			if routeExisted && !bytes.Equal(mustRouteBytes(inv, old), func() []byte { b, _ := r.routeBytes(token); return b }()) {
				return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
			}
			if e = r.removeOwned(ctx, inv, candidate); e != nil {
				return hostprotocol.Result{}, hostcontract.StableObservation{}, e
			}
			if e = r.removeEnv(candidate.Env); e != nil {
				return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
			}
			return hostprotocol.Result{}, hostcontract.StableObservation{}, operationFailed()
		}
		if old.Name != "" {
			if e = r.drainAndRemoveApp(ctx, inv, old, s.Journal != nil && s.Journal.Status == journalPending && s.Journal.Key == requestKey(q)); e != nil {
				return hostprotocol.Result{}, hostcontract.StableObservation{}, e
			}
			if old.Env != "" && old.Env != candidate.Env {
				if e = r.removeEnv(old.Env); e != nil {
					return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
				}
			}
		}
		kept = append(kept, candidate)
	}
	inv.Objects, inv.AppliedRevision = kept, q.TargetRevision
	if e = r.writeInventory(inv); e != nil {
		return hostprotocol.Result{}, hostcontract.StableObservation{}, operationFailed()
	}
	obs := hostcontract.StableObservation{Machine: s.Machine, Ownership: s.Ownership, HostRelease: q.Target.ReleaseArtifact, AppliedRevision: q.TargetRevision, Ready: true}
	for _, a := range q.Target.Apps {
		obs.Apps = append(obs.Apps, hostcontract.AppObservation{ID: a.ID, ActiveImage: a.Image, Ready: true})
	}
	obs.Data = append(obs.Data, localObservations(data)...)
	if !coversTarget(obs, q.Target.Apps) || !exactDataObservations(obs.Data, q.Target.DataServices, s) {
		return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
	}
	return hostprotocol.Result{Status: hostprotocol.ResultApplied, AppliedRevision: q.TargetRevision}, obs, nil
}

func localDataToken(id string) string { return token("local-data", id) }
func (r *Runtime) reconcileLocal(ctx context.Context, s State, inv inventory, q hostprotocol.Request) ([]managedObject, managedObject, error) {
	kept := make([]managedObject, 0, len(q.Target.DataServices))
	for _, old := range inv.Objects {
		if old.Role == "local-data-meta" {
			kept = append(kept, old)
		}
	}
	wanted := map[string]bool{}
	for _, t := range q.Target.DataServices {
		idToken := localDataToken(t.ID)
		wanted[idToken] = true
		old := findLocalData(inv, idToken)
		metadata := findLocalDataMetadata(inv, idToken)
		if old.Name == "" {
			old = metadata
		}
		if old.Name != "" && old.Type != t.Type {
			return nil, managedObject{}, recovery()
		}
		if old.Name == "" && old.Type != "" && old.Type != t.Type {
			return nil, managedObject{}, recovery()
		}
		o := localObject(s, t, q.TargetRevision)
		if old.Type != "" {
			o.DataToken, o.PathToken = old.DataToken, old.PathToken
			if localShellEqual(old, o, t) && (old.Type == "postgres" || r.redisConfigMatches(old, t, (*q.Secrets).LocalDataServices[t.ID])) {
				o = old
			}
		}
		if o.DataToken == "" {
			o.DataToken, o.PathToken = token("data", idToken), token("path", idToken)
		}
		if err := r.ensureDataDir(o.DataToken); err != nil {
			return nil, managedObject{}, operationFailed()
		}
		sameShell := old.Name != "" && old.Image == o.Image && old.Port == o.Port && old.Persistence == o.Persistence && publicationEqual(old.Bindings, t.Bindings)
		sameShell = sameShell && sameClients(old.Clients, t.Clients)
		if old.Type == "redis" {
			sameShell = sameShell && r.redisConfigMatches(old, t, (*q.Secrets).LocalDataServices[t.ID])
		}
		if sameShell && r.inspectOwned(ctx, inv, old) == nil && r.exactLocalPublications(ctx, old) == nil && r.localSecurityArtifactsValid(old) && r.localReady(ctx, old) == nil {
			changed := false
			if old.Type == "postgres" {
				var err error
				changed, err = r.postgresPasswordChanged(old, (*q.Secrets).LocalDataServices[t.ID].AdminPassword)
				if err != nil {
					return nil, managedObject{}, recovery()
				}
			}
			if err := r.reconcileDataClients(ctx, s, inv, old, q.TargetRevision, t, (*q.Secrets).LocalDataServices[t.ID]); err != nil {
				return nil, managedObject{}, err
			}
			if old.Type == "postgres" && changed {
				if err := r.writeLocalSecrets(ctx, s, old, t, (*q.Secrets).LocalDataServices[t.ID]); err != nil {
					return nil, managedObject{}, operationFailed()
				}
			}
			old.Clients, old.Bindings = append([]hostcontract.LocalDataClient(nil), t.Clients...), append([]hostcontract.LocalDataBinding(nil), t.Bindings...)
			kept = append(kept, old)
			continue
		}
		pending := s.Journal != nil && s.Journal.Status == journalPending && s.Journal.Key == requestKey(q)
		exists := false
		if pending {
			var candidateErr error
			exists, candidateErr = r.candidateExists(ctx, inv, o)
			if candidateErr != nil && !isConflict(candidateErr) {
				return nil, managedObject{}, candidateErr
			}
		}
		oldPresent := false
		if old.Name != "" && !exists {
			changed := false
			if old.Type == "postgres" && old.Revision != o.Revision {
				var passwordErr error
				changed, passwordErr = r.postgresPasswordChanged(old, (*q.Secrets).LocalDataServices[t.ID].AdminPassword)
				if passwordErr != nil {
					return nil, managedObject{}, recovery()
				}
			}
			var oldErr error
			oldPresent, oldErr = r.ownedPresent(ctx, inv, old)
			if oldErr != nil {
				return nil, managedObject{}, oldErr
			}
			if !oldPresent && changed && !pending {
				return nil, managedObject{}, recovery()
			}
			if changed && oldPresent {
				sql, err := postgresPasswordSQL((*q.Secrets).LocalDataServices[t.ID].AdminPassword)
				if err != nil {
					return nil, managedObject{}, recovery()
				}
				if _, err = r.runner.Run(ctx, []string{"exec", "-i", old.Name, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "s2h_admin", "-d", "postgres"}, sql); err != nil {
					return nil, managedObject{}, operationFailed()
				}
			}
			if oldPresent {
				if err := r.removeLocalProgress(ctx, inv, old, true); err != nil {
					return nil, managedObject{}, err
				}
			}
		}
		if !exists {
			var candidateErr error
			exists, candidateErr = r.candidateExists(ctx, inv, o)
			if candidateErr != nil {
				return nil, managedObject{}, candidateErr
			}
		}
		if err := r.writeLocalSecrets(ctx, s, o, t, (*q.Secrets).LocalDataServices[t.ID]); err != nil {
			return nil, managedObject{}, operationFailed()
		}
		if !exists {
			if err := r.runLocal(ctx, s, o, t); err != nil {
				if exists, observed := r.candidateExists(ctx, inv, o); observed != nil {
					return nil, managedObject{}, observed
				} else if !exists {
					return nil, managedObject{}, operationFailed()
				}
				return nil, managedObject{}, operationFailed()
			}
		}
		if err := r.localReady(ctx, o); err != nil {
			return nil, managedObject{}, operationFailed()
		}
		if err := r.exactLocalPublications(ctx, o); err != nil {
			return nil, managedObject{}, err
		}
		if err := r.reconcileDataClients(ctx, s, inv, o, q.TargetRevision, t, (*q.Secrets).LocalDataServices[t.ID]); err != nil {
			return nil, managedObject{}, err
		}
		if old.Name != "" && old.Env != "" && old.Env != o.Env {
			if err := r.removeEnv(old.Env); err != nil {
				return nil, managedObject{}, recovery()
			}
		}
		if old.Name != "" && old.Config != "" && old.Config != o.Config {
			if err := r.removeEnv(old.Config); err != nil {
				return nil, managedObject{}, recovery()
			}
		}
		if old.Name != "" && old.HBA != "" && old.HBA != o.HBA {
			if err := r.removeEnv(old.HBA); err != nil {
				return nil, managedObject{}, recovery()
			}
		}
		if old.Name != "" && old.Ident != "" && old.Ident != o.Ident {
			if err := r.removeEnv(old.Ident); err != nil {
				return nil, managedObject{}, recovery()
			}
		}
		kept = append(kept, o)
		if metadata.Type != "" {
			for i := range kept {
				if kept[i].Role == "local-data-meta" && kept[i].AppToken == idToken {
					kept = append(kept[:i], kept[i+1:]...)
					break
				}
			}
		}
	}
	for _, old := range inv.Objects {
		if old.Role == "local-data" && !wanted[old.AppToken] {
			if err := r.removeLocalProgress(ctx, inv, old, true); err != nil {
				return nil, managedObject{}, err
			}
			if err := r.removeEnv(old.Env); err != nil {
				return nil, managedObject{}, recovery()
			}
			if old.Config != "" {
				if err := r.removeEnv(old.Config); err != nil {
					return nil, managedObject{}, recovery()
				}
			}
			if old.HBA != "" {
				if err := r.removeEnv(old.HBA); err != nil {
					return nil, managedObject{}, recovery()
				}
			}
			if old.Ident != "" {
				if err := r.removeEnv(old.Ident); err != nil {
					return nil, managedObject{}, recovery()
				}
			}
			kept = append(kept, managedObject{Role: "local-data-meta", AppToken: old.AppToken, Type: old.Type, DataToken: old.DataToken, PathToken: old.PathToken, DataIdentity: old.DataIdentity})
		}
	}
	proxy := findProxy(inv)
	if q.Target.ReverseProxy == nil {
		if proxy.Name != "" {
			if err := r.removeOwnedProgress(ctx, inv, proxy, true); err != nil {
				return nil, managedObject{}, err
			}
			if err := r.removeEnv(proxy.Env); err != nil {
				return nil, managedObject{}, recovery()
			}
			if err := r.removeEnv(proxy.Config); err != nil {
				return nil, managedObject{}, recovery()
			}
		}
		return kept, managedObject{}, nil
	}
	next := proxyObject(s, *q.Target.ReverseProxy, q.TargetRevision)
	if err := r.ensureProxyPaths(); err != nil {
		return nil, managedObject{}, operationFailed()
	}
	if proxy.Name != "" && proxy.Image == next.Image && proxy.Revision == next.Revision && r.inspectOwned(ctx, inv, proxy) == nil && r.proxyReady(ctx, proxy) == nil {
		return kept, proxy, nil
	}
	pending := s.Journal != nil && s.Journal.Status == journalPending && s.Journal.Key == requestKey(q)
	exists := false
	if pending {
		var candidateErr error
		exists, candidateErr = r.candidateExists(ctx, inv, next)
		if candidateErr != nil && !isConflict(candidateErr) {
			return nil, managedObject{}, candidateErr
		}
	}
	if proxy.Name != "" && !exists {
		if err := r.removeOwnedProgress(ctx, inv, proxy, true); err != nil {
			return nil, managedObject{}, err
		}
	}
	if !exists {
		var candidateErr error
		exists, candidateErr = r.candidateExists(ctx, inv, next)
		if candidateErr != nil {
			return nil, managedObject{}, candidateErr
		}
	}
	if err := r.writeArtifact(next.Env, []byte("CF_DNS_API_TOKEN="+(*q.Secrets).ReverseProxy.DNSChallengeToken+"\nACME_EMAIL="+q.Target.ReverseProxy.ACMEEmail+"\n"), 0600); err != nil {
		return nil, managedObject{}, err
	}
	if err := r.writeArtifact(next.Config, traefikStaticConfig(q.Target.ReverseProxy.ACMEEmail), 0600); err != nil {
		return nil, managedObject{}, err
	}
	if !exists {
		if err := r.runProxy(ctx, s, next); err != nil {
			if exists, observed := r.candidateExists(ctx, inv, next); observed != nil {
				return nil, managedObject{}, observed
			} else if !exists {
				return nil, managedObject{}, operationFailed()
			}
			return nil, managedObject{}, operationFailed()
		}
	}
	if err := r.proxyReady(ctx, next); err != nil {
		return nil, managedObject{}, operationFailed()
	}
	if proxy.Name != "" && proxy.Env != next.Env {
		if err := r.removeEnv(proxy.Env); err != nil {
			return nil, managedObject{}, recovery()
		}
	}
	if proxy.Name != "" && proxy.Config != next.Config {
		if err := r.removeEnv(proxy.Config); err != nil {
			return nil, managedObject{}, recovery()
		}
	}
	return kept, next, nil
}
func localObject(s State, t hostcontract.LocalDataServiceTarget, revision string) managedObject {
	idToken := localDataToken(t.ID)
	image := postgresImage
	database := "sub2api"
	tls := nameForLocal(s, idToken)
	if t.Type == "redis" {
		image, database, tls = redisImage, "0", ""
	}
	name := nameForLocal(s, idToken)
	o := managedObject{Role: "local-data", AppToken: idToken, Service: t.ID, Name: name, Image: image, Revision: revision, Type: t.Type, Port: t.Port, Persistence: t.Persistence, Env: envName(idToken, revision), DataIdentity: hostcontract.DataIdentity{Kind: t.Type, ProviderID: name, Endpoint: name, Port: t.Port, Database: database, TLSServerName: tls}, Bindings: append([]hostcontract.LocalDataBinding(nil), t.Bindings...), Clients: append([]hostcontract.LocalDataClient(nil), t.Clients...)}
	if t.Type == "redis" {
		o.Config = artifactConfigPrefix + token(idToken, revision)
	} else {
		o.Config = artifactConfigPrefix + token(idToken, revision, "postgresql")
		o.HBA = artifactConfigPrefix + token(idToken, revision, "hba")
		o.Ident = artifactConfigPrefix + token(idToken, revision, "ident")
	}
	return o
}
func appObject(s State, a hostcontract.AppTarget, revision, active string) managedObject {
	token := appToken(a.ID)
	return managedObject{Role: "app", AppToken: token, Name: objectName(s, "app", token, active), Image: a.Image, Data: links(a), Revision: revision, Active: active, Env: envName(token, revision), Hostname: a.Hostname, ReadinessPath: a.ReadinessPath, DrainSeconds: appDrainSeconds(a)}
}
func appDrainSeconds(a hostcontract.AppTarget) int {
	v := a.DrainTimeout
	if v == "" {
		v = "30s"
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 || d%time.Second != 0 || d > 10*time.Minute {
		return 0
	}
	return int(d / time.Second)
}
func nameForLocal(s State, token string) string { return objectName(s, "local-data", token, "live") }
func findProxy(i inventory) managedObject {
	for _, o := range i.Objects {
		if o.Role == "proxy" {
			return o
		}
	}
	return managedObject{}
}
func proxyObject(s State, t hostcontract.ReverseProxyTarget, revision string) managedObject {
	proxyToken := token("proxy")
	return managedObject{Role: "proxy", Name: objectName(s, "proxy", "proxy", "live"), Image: t.Image, Revision: revision, Env: envName(proxyToken, revision), Config: artifactConfigPrefix + token("proxy", revision)}
}
func localObservations(objects []managedObject) []hostcontract.DataObservation {
	var values []hostcontract.DataObservation
	for _, o := range objects {
		if o.Role == "local-data" {
			values = append(values, hostcontract.DataObservation{Identity: o.DataIdentity, Ready: true})
		}
	}
	return values
}
func coversTarget(obs hostcontract.StableObservation, apps []hostcontract.AppTarget) bool {
	if !obs.Ready || len(obs.Apps) != len(apps) {
		return false
	}
	seen := map[string]hostcontract.AppObservation{}
	for _, app := range obs.Apps {
		if !app.Ready {
			return false
		}
		seen[app.ID] = app
	}
	for _, target := range apps {
		app, ok := seen[target.ID]
		if !ok || app.ActiveImage != target.Image {
			return false
		}
	}
	return true
}
func findApp(inv inventory, token string) managedObject {
	for _, o := range inv.Objects {
		if o.Role == "app" && o.AppToken == token {
			return o
		}
	}
	return managedObject{}
}
func links(a hostcontract.AppTarget) []managedLink {
	var x []managedLink
	for _, l := range a.DataLinks {
		x = append(x, managedLink{Name: l.Name, Identity: l.Identity})
	}
	return x
}
func envName(app, revision string) string { return artifactEnvPrefix + app + token(revision) }
func envBytes(a hostcontract.AppTarget, s hostcontract.AppSecrets) []byte {
	var b strings.Builder
	values := make(map[string]string, len(a.RuntimeSettings)+len(s.RuntimeEnvironment))
	for k, v := range a.RuntimeSettings {
		values[k] = v
	}
	for k, v := range s.RuntimeEnvironment {
		values[k] = v
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := values[k]
		b.WriteString(k + "=" + v + "\n")
	}
	b.WriteString("ADMIN_EMAIL=" + a.InitialAdminEmail + "\n")
	for _, item := range []struct{ name, value string }{{"INITIAL_ADMIN_PASSWORD", s.InitialAdminPassword}, {"JWT_SECRET", s.JWTSecret}, {"TOTP_ENCRYPTION_KEY", s.TOTPEncryptionKey}, {"ADMIN_API_KEY", s.AdminAPIKey}} {
		if item.value != "" {
			b.WriteString(item.name + "=" + item.value + "\n")
		}
	}
	for _, link := range a.DataLinks {
		switch link.Identity.Kind {
		case "postgres":
			if s.Postgres != nil {
				b.WriteString("DATABASE_HOST=" + link.Identity.Endpoint + "\nDATABASE_PORT=" + strconv.Itoa(link.Identity.Port) + "\nDATABASE_USER=" + s.Postgres.Username + "\nDATABASE_PASSWORD=" + s.Postgres.Password + "\nDATABASE_DBNAME=" + link.Identity.Database + "\nDATABASE_SSLMODE=" + link.Identity.TLSMode + "\n")
			}
		case "redis":
			if s.Redis != nil {
				b.WriteString("REDIS_HOST=" + link.Identity.Endpoint + "\nREDIS_PORT=" + strconv.Itoa(link.Identity.Port) + "\nREDIS_USERNAME=" + s.Redis.Username + "\nREDIS_PASSWORD=" + s.Redis.Password + "\nREDIS_DB=" + link.Identity.Database + "\nREDIS_ENABLE_TLS=" + strconv.FormatBool(link.Identity.TLSMode == "require") + "\n")
			}
		}
	}
	return []byte(b.String())
}
func (r *Runtime) Retire(ctx context.Context, q hostprotocol.Request) (hostprotocol.Result, error) {
	if q.Action != hostcontract.ActionRetirePreserveData || q.Approval == nil {
		return hostprotocol.Result{}, approvalRequired()
	}
	key := requestKey(q)
	if q.Approval.Validate() != nil || !q.Approval.Matches(key, "") {
		return hostprotocol.Result{}, approvalRequired()
	}
	s, e := r.readState()
	if e != nil || r.validateLiveState(s, q.Resource) != nil || q.Approval.Machine != s.Machine || q.Approval.Ownership != s.Ownership {
		return hostprotocol.Result{}, recovery()
	}
	if result, ok := r.terminalResult(key); ok {
		return result, nil
	}
	inv, e := r.readInventory()
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return hostprotocol.Result{}, recovery()
	}
	if e == nil {
		if validateInventoryForState(inv, s) != nil || r.checkKnownOwned(ctx, inv, s.Journal != nil && s.Journal.Status == journalPending && s.Journal.Key == key) != nil {
			return hostprotocol.Result{}, recovery()
		}
	}
	if err := r.admitRetirementNft(ctx, s, inv); err != nil {
		return hostprotocol.Result{}, err
	}
	return r.RunOperation(requestKey(q), q.Approval, func(op *Operation) (hostprotocol.Result, hostcontract.StableObservation, error) {
		inv, e := r.readInventory()
		if e == nil {
			if validateInventoryForState(inv, op.state) != nil {
				return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
			}
			for _, o := range inv.Objects {
				if o.Role == "app" {
					if e = r.removeRouteProgress(inv, o, true); e != nil {
						return hostprotocol.Result{}, hostcontract.StableObservation{}, e
					}
				}
			}
			for _, role := range []string{"app", "proxy", "local-data"} {
				for _, o := range inv.Objects {
					if o.Role != role || o.Name == "" {
						continue
					}
					if role == "local-data" {
						e = r.removeLocalProgress(ctx, inv, o, true)
					} else {
						e = r.removeOwnedProgress(ctx, inv, o, true)
					}
					if e != nil {
						return hostprotocol.Result{}, hostcontract.StableObservation{}, e
					}
					if o.Env != "" {
						if e = r.removeEnv(o.Env); e != nil {
							return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
						}
					}
					if o.Config != "" {
						if e = r.removeEnv(o.Config); e != nil {
							return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
						}
					}
					if o.HBA != "" {
						if e = r.removeEnv(o.HBA); e != nil {
							return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
						}
					}
					if o.Ident != "" {
						if e = r.removeEnv(o.Ident); e != nil {
							return hostprotocol.Result{}, hostcontract.StableObservation{}, recovery()
						}
					}
				}
			}
		}
		if e = r.removeNft(ctx, op.state, true); e != nil {
			return hostprotocol.Result{}, hostcontract.StableObservation{}, e
		}
		if e = r.removeNetworkProgress(ctx, op.state, true); e != nil {
			return hostprotocol.Result{}, hostcontract.StableObservation{}, e
		}
		return hostprotocol.Result{Status: hostprotocol.ResultRetired, Machine: &op.state.Machine, Ownership: &op.state.Ownership, Retirement: &hostprotocol.RetirementEvidence{PreserveData: true}}, hostcontract.StableObservation{}, nil
	})
}
func (r *Runtime) admitRetirementNft(ctx context.Context, s State, inv inventory) error {
	targets := make([]hostcontract.LocalDataServiceTarget, 0)
	for _, object := range inv.Objects {
		if object.Role == "local-data" {
			targets = append(targets, hostcontract.LocalDataServiceTarget{ID: object.Service, Type: object.Type, Port: object.Port, Bindings: object.Bindings})
		}
	}
	policy, err := nftPolicyForTargets(targets)
	if err != nil {
		return recovery()
	}
	observed, err := r.nft.Run(ctx, []string{"-j", "list", "table", "inet", nftTableName(s)}, nil)
	if errors.Is(err, errNftNotFound) {
		return nil
	}
	if err != nil {
		return recovery()
	}
	switch classifyNftJSON(observed, s, policy) {
	case nftExact, nftOld:
		return nil
	case nftForeign:
		return conflict()
	default:
		return recovery()
	}
}
func (r *Runtime) removeNetworkProgress(ctx context.Context, s State, allowAbsent bool) error {
	if err := r.admitNetwork(ctx, s); err != nil {
		return err
	}
	out, err := r.runner.Run(ctx, networkListArgs(s), nil)
	if err != nil {
		return recovery()
	}
	if strings.TrimSpace(string(out)) == "" {
		if allowAbsent {
			return nil
		}
		return recovery()
	}
	if err := r.docker(ctx, "network", "rm", networkName(s)); err != nil {
		if removeErr := r.admitNetwork(ctx, s); removeErr == nil {
			out, removeErr = r.runner.Run(ctx, networkListArgs(s), nil)
			if removeErr == nil && strings.TrimSpace(string(out)) == "" {
				return nil
			}
		}
		return recovery()
	}
	return nil
}
func mustRouteBytes(inv inventory, o managedObject) []byte {
	b, _ := routeBytesFor(inv, o)
	return b
}
func (r *Runtime) removeOwned(ctx context.Context, inv inventory, o managedObject) error {
	if e := r.inspectOwned(ctx, inv, o); e != nil {
		return e
	}
	return r.docker(ctx, "rm", "-f", o.Name)
}
func (r *Runtime) removeOwnedProgress(ctx context.Context, inv inventory, o managedObject, allowAbsent bool) error {
	present, err := r.ownedPresent(ctx, inv, o)
	if err != nil {
		return err
	}
	if !present {
		if allowAbsent {
			return nil
		}
		return recovery()
	}
	return r.docker(ctx, "rm", "-f", o.Name)
}
func (r *Runtime) removeLocalProgress(ctx context.Context, inv inventory, o managedObject, allowAbsent bool) error {
	if err := r.removeOwnedProgress(ctx, inv, o, allowAbsent); err != nil {
		return err
	}
	present, err := r.ownedPresent(ctx, inv, o)
	if err != nil || present {
		return recovery()
	}
	return nil
}
func (r *Runtime) drainAndRemoveApp(ctx context.Context, inv inventory, o managedObject, allowAbsent bool) error {
	present, err := r.ownedPresent(ctx, inv, o)
	if err != nil {
		return err
	}
	if !present {
		if allowAbsent {
			return nil
		}
		return recovery()
	}
	seconds := o.DrainSeconds
	if seconds == 0 {
		seconds = 30
	}
	if err = r.docker(ctx, "stop", "--time", strconv.Itoa(seconds), o.Name); err != nil {
		// A prior stop may have completed while its response was lost. Reinspect
		// ownership, then leave the pending operation for a safe retry rather
		// than force-removing a possibly active app.
		if err = r.inspectOwned(ctx, inv, o); err != nil {
			return err
		}
		return operationFailed()
	}
	return r.removeOwnedProgress(ctx, inv, o, allowAbsent)
}
func (r *Runtime) docker(ctx context.Context, args ...string) error {
	_, e := r.runner.Run(ctx, args, nil)
	return e
}
func (r *Runtime) ready(ctx context.Context, name, path string) error {
	return r.docker(ctx, "exec", name, "wget", "-q", "-O", "/dev/null", "http://localhost:8080"+path)
}
func (r *Runtime) postRouteReady(ctx context.Context, proxy, candidate managedObject, path string) error {
	if proxy.Name == "" {
		return r.ready(ctx, candidate.Name, path)
	}
	return r.docker(ctx, "exec", candidate.Name, "wget", "-q", "-O", "/dev/null", "--header", "Host:"+candidate.Hostname, "http://"+proxy.Name+":8081"+path)
}
func (r *Runtime) localReady(ctx context.Context, o managedObject) error {
	if o.Type == "postgres" {
		return r.docker(ctx, "exec", o.Name, "psql", "-X", "-U", "s2h_admin", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", "SELECT 1")
	}
	// REDISCLI_AUTH is loaded by docker run's --env-file; never route it through
	// a shell, URI, option, or command argument.
	out, err := r.runner.Run(ctx, []string{"exec", o.Name, "redis-cli", "--raw", "-h", "127.0.0.1", "-p", strconv.Itoa(o.Port), "ping"}, nil)
	if err != nil || !bytes.Equal(out, []byte("PONG\n")) {
		return operationFailed()
	}
	return nil
}

// exactDockerPublications accepts only the fixed `docker container inspect
// --format {{json .HostConfig.PortBindings}}` envelope used below. Docker's
// port map is part of the security boundary, so labels never substitute for it.
func exactDockerPublications(raw []byte, port int, bindings []hostcontract.LocalDataBinding) bool {
	var document struct {
		HostConfig struct {
			PortBindings map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"PortBindings"`
		} `json:"HostConfig"`
	}
	if strictJSON(raw, &document) != nil {
		return false
	}
	if len(bindings) == 0 {
		return len(document.HostConfig.PortBindings) == 0
	}
	if len(document.HostConfig.PortBindings) != 1 {
		return false
	}
	entries, ok := document.HostConfig.PortBindings[strconv.Itoa(port)+"/tcp"]
	if !ok || len(entries) != len(bindings) {
		return false
	}
	want := map[string]bool{}
	for _, binding := range bindings {
		ip := net.ParseIP(binding.Address)
		if ip == nil || binding.Address != ip.String() {
			return false
		}
		want[binding.Address] = true
	}
	for _, entry := range entries {
		ip := net.ParseIP(entry.HostIP)
		if ip == nil || entry.HostIP != ip.String() || ip.IsUnspecified() || entry.HostPort != strconv.Itoa(port) || !want[entry.HostIP] {
			return false
		}
		delete(want, entry.HostIP)
	}
	return len(want) == 0
}

func (r *Runtime) exactLocalPublications(ctx context.Context, o managedObject) error {
	out, err := r.runner.Run(ctx, []string{"container", "inspect", "--format", `{"HostConfig":{"PortBindings":{{json .HostConfig.PortBindings}}}}`, o.Name}, nil)
	if err != nil || !exactDockerPublications(out, o.Port, o.Bindings) {
		return recovery()
	}
	return nil
}
func (r *Runtime) writeLocalSecrets(ctx context.Context, s State, o managedObject, t hostcontract.LocalDataServiceTarget, secret hostcontract.LocalDataServiceSecrets) error {
	if strings.ContainsAny(secret.AdminPassword, "\x00\n\r") {
		return errors.New("secret")
	}
	if o.Type == "postgres" {
		if err := r.writeArtifact(o.Env, []byte(secret.AdminPassword+"\n"), 0600); err != nil {
			return err
		}
		if err := r.writeArtifact(o.Config, []byte("listen_addresses = '*'\npassword_encryption = 'scram-sha-256'\nhba_file = '/etc/sub2api/pg_hba.conf'\nident_file = '/etc/sub2api/pg_ident.conf'\n"), 0600); err != nil {
			return err
		}
		var hba strings.Builder
		hba.WriteString("local all all peer map=s2h_admin\n")
		for _, client := range t.Clients {
			if !validPostgresClient(client) {
				return errors.New("postgres client")
			}
			hba.WriteString("host " + client.Database + " " + client.Username + " all scram-sha-256\n")
		}
		hba.WriteString("local all all reject\nhost all all all reject\n")
		if err := r.writeArtifact(o.HBA, []byte(hba.String()), 0600); err != nil {
			return err
		}
		return r.writeArtifact(o.Ident, []byte("s2h_admin root s2h_admin\ns2h_admin postgres s2h_admin\n"), 0600)
	}
	config, err := redisConfig(t, secret)
	if err != nil {
		return err
	}
	if err := r.writeArtifact(o.Config, []byte(config), 0600); err != nil {
		return err
	}
	return r.writeArtifact(o.Env, []byte("REDISCLI_AUTH="+secret.AdminPassword+"\n"), 0600)
}
func redisConfig(t hostcontract.LocalDataServiceTarget, secret hostcontract.LocalDataServiceSecrets) (string, error) {
	password, err := redisConfigValue(secret.AdminPassword)
	if err != nil {
		return "", err
	}
	config := "port " + strconv.Itoa(t.Port) + "\ndir /data\nrequirepass " + password + "\nuser default on >" + password + " ~* &* +@all\n"
	for _, client := range t.Clients {
		clientPassword, err := redisConfigValue(secret.ClientPasswords[client.AppID])
		if err != nil || !validPrincipal(client.Username) {
			return "", errors.New("redis client")
		}
		config += "user " + client.Username + " on >" + clientPassword + " ~* &* +@all\n"
	}
	if t.Persistence {
		return config + "appendonly yes\nsave 60 1\n", nil
	}
	return config + "appendonly no\nsave \"\"\n", nil
}
func (r *Runtime) redisConfigMatches(old managedObject, target hostcontract.LocalDataServiceTarget, secret hostcontract.LocalDataServiceSecrets) bool {
	want, err := redisConfig(target, secret)
	if err != nil {
		return false
	}
	got, err := r.readArtifactBytes(old.Config)
	return err == nil && string(got) == want
}
func (r *Runtime) localSecurityArtifactsValid(o managedObject) bool {
	if o.Type == "redis" {
		_, err := r.readArtifactBytes(o.Config)
		return err == nil
	}
	config, configErr := r.readArtifactBytes(o.Config)
	hba, hbaErr := r.readArtifactBytes(o.HBA)
	ident, identErr := r.readArtifactBytes(o.Ident)
	if configErr != nil || hbaErr != nil || identErr != nil {
		return false
	}
	if string(config) != "listen_addresses = '*'\npassword_encryption = 'scram-sha-256'\nhba_file = '/etc/sub2api/pg_hba.conf'\nident_file = '/etc/sub2api/pg_ident.conf'\n" || string(ident) != "s2h_admin root s2h_admin\ns2h_admin postgres s2h_admin\n" {
		return false
	}
	var expected strings.Builder
	expected.WriteString("local all all peer map=s2h_admin\n")
	for _, client := range o.Clients {
		if !validPostgresClient(client) {
			return false
		}
		expected.WriteString("host " + client.Database + " " + client.Username + " all scram-sha-256\n")
	}
	expected.WriteString("local all all reject\nhost all all all reject\n")
	return string(hba) == expected.String()
}
func (r *Runtime) dockerNetworkCIDRs(ctx context.Context, s State) ([]string, error) {
	out, err := r.runner.Run(ctx, []string{"network", "inspect", "--format", "{{range .IPAM.Config}}{{.Subnet}} {{end}}", networkName(s)}, nil)
	if err != nil {
		return nil, recovery()
	}
	seen := map[string]bool{}
	var cidrs []string
	for _, value := range strings.Fields(string(out)) {
		ip, network, parseErr := net.ParseCIDR(value)
		if parseErr != nil || ip.String() != network.IP.String() || seen[value] {
			return nil, recovery()
		}
		seen[value] = true
		cidrs = append(cidrs, value)
	}
	if len(cidrs) == 0 {
		return nil, recovery()
	}
	sort.Strings(cidrs)
	return cidrs, nil
}
func sameBindings(a, b []hostcontract.LocalDataBinding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Address != b[i].Address || !sameStringSlice(a[i].AllowedSources, b[i].AllowedSources) {
			return false
		}
	}
	return true
}
func publicationEqual(a, b []hostcontract.LocalDataBinding) bool {
	if len(a) != len(b) {
		return false
	}
	addresses := map[string]bool{}
	for _, binding := range a {
		addresses[binding.Address] = true
	}
	for _, binding := range b {
		if !addresses[binding.Address] {
			return false
		}
	}
	return true
}
func localShellEqual(old, candidate managedObject, target hostcontract.LocalDataServiceTarget) bool {
	return old.Name != "" && old.Image == candidate.Image && old.Type == candidate.Type && old.Port == candidate.Port && old.Persistence == candidate.Persistence && publicationEqual(old.Bindings, target.Bindings) && sameClients(old.Clients, target.Clients)
}
func nftUnion(inv inventory, desired []hostcontract.LocalDataServiceTarget) []hostcontract.LocalDataServiceTarget {
	bySocket := map[string]hostcontract.LocalDataBinding{}
	add := func(port int, bindings []hostcontract.LocalDataBinding) {
		for _, binding := range bindings {
			key := binding.Address + "\x00" + strconv.Itoa(port)
			current := bySocket[key]
			current.Address = binding.Address
			seen := map[string]bool{}
			for _, source := range current.AllowedSources {
				seen[source] = true
			}
			for _, source := range binding.AllowedSources {
				if seen[source] {
					continue
				}
				current.AllowedSources, seen[source] = append(current.AllowedSources, source), true
			}
			sort.Strings(current.AllowedSources)
			bySocket[key] = current
		}
	}
	for _, object := range inv.Objects {
		if object.Role == "local-data" {
			add(object.Port, object.Bindings)
		}
	}
	for _, target := range desired {
		add(target.Port, target.Bindings)
	}
	var result []hostcontract.LocalDataServiceTarget
	for key, binding := range bySocket {
		port, _ := strconv.Atoi(key[strings.LastIndex(key, "\x00")+1:])
		result = append(result, hostcontract.LocalDataServiceTarget{ID: key, Port: port, Bindings: []hostcontract.LocalDataBinding{binding}})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
func sameClients(a, b []hostcontract.LocalDataClient) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func validPrincipal(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	for i, c := range value {
		if !(c == '_' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func sqlIdentifier(value string) (string, error) {
	if !validPrincipal(value) {
		return "", errors.New("identifier")
	}
	return value, nil
}

func validPostgresClient(client hostcontract.LocalDataClient) bool {
	return validClientAppID(client.AppID) && validPrincipal(client.Username) && validPrincipal(client.Database) && !strings.HasPrefix(client.Username, "s2h_") && client.Database != "postgres"
}

func postgresOwner(serviceID, database string) string {
	sum := sha256.Sum256([]byte("sub2api-host-postgres-owner-v1\x00" + serviceID + "\x00" + database))
	return "s2h_owner_" + hex.EncodeToString(sum[:12])
}

// postgresCopyValue writes one text-COPY field without allowing a password to
// terminate the data stream or alter a psql command.
func postgresCopyValue(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n", "\r", "\\r").Replace(value)
}

func postgresBinding(s State, serviceID string) string {
	return token(s.Resource.Environment, s.Resource.ServerKey, s.Ownership.Value, serviceID)
}
func postgresOperation(revision string) string { return token(revision) }
func postgresRolesMarker(s State, serviceID, revision string) string {
	return "s2hpg2:" + postgresBinding(s, serviceID) + ":roles:" + postgresOperation(revision) + ":admin"
}
func postgresOwnerMarker(s State, serviceID, revision, database string) string {
	return "s2hpg2:" + postgresBinding(s, serviceID) + ":roles:" + postgresOperation(revision) + ":owner:" + token(database)
}
func postgresClientMarker(s State, serviceID, revision, appID string) string {
	return "s2hpg2:" + postgresBinding(s, serviceID) + ":roles:" + postgresOperation(revision) + ":client:" + token(appID)
}
func postgresDatabaseMarker(s State, serviceID, revision, database string) string {
	return "s2hpg2:" + postgresBinding(s, serviceID) + ":database:" + postgresOperation(revision) + ":" + token(database)
}

func postgresClientSQL(s State, serviceID, revision string, clients []hostcontract.LocalDataClient, passwords map[string]string, previous []hostcontract.LocalDataClient, adminPassword string) (string, error) {
	if !validPrincipal(serviceID) || revision == "" {
		return "", errors.New("postgres target")
	}
	seenApps, desired := map[string]bool{}, map[string]bool{}
	for _, client := range clients {
		if !validPostgresClient(client) || seenApps[client.AppID] || !validLocalPassword(passwords[client.AppID]) {
			return "", errors.New("postgres client")
		}
		seenApps[client.AppID], desired[client.Username] = true, true
	}
	if !validLocalPassword(adminPassword) {
		return "", errors.New("postgres admin password")
	}
	var b strings.Builder
	b.WriteString("BEGIN;\nCREATE TEMP TABLE s2h_clients(app_id text NOT NULL, username text NOT NULL, database_name text NOT NULL, owner_name text NOT NULL, password text NOT NULL);\nCOPY s2h_clients FROM STDIN;\n")
	for _, client := range clients {
		b.WriteString(postgresCopyValue(client.AppID) + "\t" + postgresCopyValue(client.Username) + "\t" + postgresCopyValue(client.Database) + "\t" + postgresCopyValue(postgresOwner(serviceID, client.Database)) + "\t" + postgresCopyValue(passwords[client.AppID]) + "\n")
	}
	b.WriteString("\\.\n")
	b.WriteString("CREATE TEMP TABLE s2h_admin(password text NOT NULL);\nCOPY s2h_admin FROM STDIN;\n" + postgresCopyValue(adminPassword) + "\n\\.\n")
	b.WriteString("SELECT format('ALTER ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L', 's2h_admin', password) FROM s2h_admin \\gexec\n")
	if len(clients) != 0 {
		b.WriteString("SELECT format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', owner_name) FROM (SELECT DISTINCT owner_name FROM s2h_clients) x WHERE NOT EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname = x.owner_name) \\gexec\n")
		b.WriteString("SELECT format('ALTER ROLE %I NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', owner_name) FROM (SELECT DISTINCT owner_name FROM s2h_clients) x \\gexec\n")
		b.WriteString("SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT PASSWORD %L', username, password) FROM (SELECT DISTINCT username, password FROM s2h_clients) c WHERE NOT EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname = c.username) \\gexec\n")
		b.WriteString("SELECT format('ALTER ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT PASSWORD %L', username, password) FROM (SELECT DISTINCT username, password FROM s2h_clients) c \\gexec\n")
		b.WriteString("SELECT format('GRANT %I TO %I WITH INHERIT FALSE SET TRUE', owner_name, username) FROM (SELECT DISTINCT owner_name, username FROM s2h_clients) c \\gexec\n")
		for _, client := range clients {
			b.WriteString("SELECT format('COMMENT ON ROLE %I IS %L', '" + postgresOwner(serviceID, client.Database) + "', '" + postgresOwnerMarker(s, serviceID, revision, client.Database) + "') \\gexec\n")
			b.WriteString("SELECT format('COMMENT ON ROLE %I IS %L', '" + client.Username + "', '" + postgresClientMarker(s, serviceID, revision, client.AppID) + "') \\gexec\n")
		}
	}
	for _, old := range previous {
		if !validPostgresClient(old) {
			return "", errors.New("previous postgres client")
		}
		if !desired[old.Username] {
			b.WriteString("SELECT format('REVOKE %I FROM %I', '" + postgresOwner(serviceID, old.Database) + "', '" + old.Username + "') \\gexec\nSELECT format('ALTER ROLE %I NOLOGIN', '" + old.Username + "') \\gexec\nSELECT format('COMMENT ON ROLE %I IS NULL', '" + old.Username + "') \\gexec\n")
		}
	}
	b.WriteString("SELECT format('COMMENT ON ROLE %I IS %L', 's2h_admin', '" + postgresRolesMarker(s, serviceID, revision) + "') \\gexec\nCOMMIT;\n")
	return b.String(), nil
}

func postgresDatabaseCreateSQL(database, owner string) string {
	return "SELECT format('CREATE DATABASE %I OWNER %I', '" + database + "', '" + owner + "') WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = '" + database + "') \\gexec\n"
}
func postgresDatabaseSQL(s State, serviceID, revision, database string, clients []hostcontract.LocalDataClient) (string, error) {
	var b strings.Builder
	for _, client := range clients {
		if !validPostgresClient(client) || client.Database != database {
			return "", errors.New("postgres database client")
		}
	}
	owner := postgresOwner(serviceID, database)
	b.WriteString("BEGIN;\nSELECT format('ALTER DATABASE %I OWNER TO %I', '" + database + "', '" + owner + "') \\gexec\nSELECT format('ALTER SCHEMA public OWNER TO %I', '" + owner + "') \\gexec\nREVOKE CREATE ON SCHEMA public FROM PUBLIC;\n")
	for _, client := range clients {
		b.WriteString("SELECT format('GRANT USAGE, CREATE ON SCHEMA public TO %I', '" + owner + "') \\gexec\nSELECT format('GRANT CONNECT ON DATABASE %I TO %I', '" + database + "', '" + client.Username + "') \\gexec\nSELECT format('ALTER ROLE %I IN DATABASE %I SET ROLE TO %I', '" + client.Username + "', '" + database + "', '" + owner + "') \\gexec\n")
	}
	b.WriteString("SELECT format('COMMENT ON DATABASE %I IS %L', '" + database + "', '" + postgresDatabaseMarker(s, serviceID, revision, database) + "') \\gexec\nCOMMIT;\n")
	return b.String(), nil
}

func (r *Runtime) reconcileDataClients(ctx context.Context, s State, inv inventory, o managedObject, revision string, target hostcontract.LocalDataServiceTarget, secret hostcontract.LocalDataServiceSecrets) error {
	if o.Type == "redis" {
		// Redis ACL state is generated in the mounted config and loaded at startup.
		return nil
	}
	expected, err := postgresCatalogExpected(s, inv, target, revision)
	if err != nil {
		return operationFailed()
	}
	return r.reconcilePostgres(ctx, s, o, expected, secret)
}
func postgresPSQLArgs(name string) []string {
	return []string{"exec", "-i", name, "psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-U", "s2h_admin", "-d", "postgres"}
}

func postgresCatalogExpected(s State, inv inventory, target hostcontract.LocalDataServiceTarget, revision string) (postgresCatalogProtocolExpected, error) {
	previous := findLocalData(inv, localDataToken(target.ID))
	e := postgresCatalogProtocolExpected{Binding: postgresCatalogProtocolBinding{Environment: s.Resource.Environment, Server: s.Resource.ServerKey, Ownership: s.Ownership.Value, Service: target.ID}, Revision: revision, Desired: append([]hostcontract.LocalDataClient(nil), target.Clients...)}
	if previous.Name != "" {
		e.Previous, e.PreviousRevision = append([]hostcontract.LocalDataClient(nil), previous.Clients...), inv.AppliedRevision
	}
	return e, e.valid()
}

func (r *Runtime) observePostgresCatalog(ctx context.Context, o managedObject, e postgresCatalogProtocolExpected) (postgresCatalogProtocolRoleObservation, []postgresCatalogProtocolDatabaseObservation, error) {
	out, err := r.runner.Run(ctx, postgresPSQLArgs(o.Name), []byte(postgresCatalogProtocolRolesSQL(e)))
	if err != nil {
		return postgresCatalogProtocolRoleObservation{}, nil, err
	}
	roles, err := parsePostgresCatalogProtocolRoles(out)
	if err != nil {
		return postgresCatalogProtocolRoleObservation{}, nil, err
	}
	var dbs []postgresCatalogProtocolDatabaseObservation
	for _, db := range postgresCatalogProtocolDatabases(e.Desired) {
		refBytes, err := r.runner.Run(ctx, postgresPSQLArgs(o.Name), []byte(postgresCatalogProtocolDatabaseReferenceSQL(e, db)))
		if err != nil {
			return postgresCatalogProtocolRoleObservation{}, nil, err
		}
		ref, err := parsePostgresCatalogProtocolReference(refBytes)
		if err != nil {
			return postgresCatalogProtocolRoleObservation{}, nil, err
		}
		var detail postgresCatalogProtocolDetailObservation
		if ref.State == postgresCatalogProtocolAbsent {
			detail, err = postgresCatalogProtocolAbsentDetail(ref)
		} else {
			detailBytes, detailErr := r.runner.Run(ctx, append(postgresPSQLArgs(o.Name), "-d", db), []byte(postgresCatalogProtocolDatabaseDetailSQL(e, db)))
			if detailErr != nil {
				return postgresCatalogProtocolRoleObservation{}, nil, detailErr
			}
			detail, err = parsePostgresCatalogProtocolDetail(detailBytes)
		}
		if err != nil {
			return postgresCatalogProtocolRoleObservation{}, nil, err
		}
		dbs = append(dbs, postgresCatalogProtocolDatabaseObservation{Key: postgresCatalogProtocolDatabaseToken(db), Reference: ref, Detail: detail})
	}
	return roles, dbs, nil
}

func postgresCatalogPending(s State, e postgresCatalogProtocolExpected) bool {
	return s.Journal != nil && s.Journal.Status == journalPending && s.Journal.Key.Action == hostcontract.ActionReconcile && s.Journal.Key.Resource == s.Resource && s.Journal.Key.TargetRevision == e.Revision
}

func (r *Runtime) postgresClassification(ctx context.Context, s State, o managedObject, e postgresCatalogProtocolExpected) (postgresCatalogProtocolClassification, error) {
	roles, dbs, err := r.observePostgresCatalog(ctx, o, e)
	if err != nil {
		return postgresCatalogProtocolClassification{}, err
	}
	return classifyPostgresCatalogProtocol(e, roles, dbs, postgresCatalogPending(s, e)), nil
}

func postgresProtocolRoleSQL(e postgresCatalogProtocolExpected, secret hostcontract.LocalDataServiceSecrets) (string, error) {
	if e.valid() != nil || !validLocalPassword(secret.AdminPassword) {
		return "", errors.New("postgres catalog writer")
	}
	for _, c := range e.Desired {
		if !validLocalPassword(secret.ClientPasswords[c.AppID]) {
			return "", errors.New("postgres client password")
		}
	}
	var b strings.Builder
	b.WriteString("BEGIN;\nCREATE TEMP TABLE s2h_clients(username text NOT NULL, database_name text NOT NULL, password text NOT NULL);\nCOPY s2h_clients FROM STDIN;\n")
	for _, c := range e.Desired {
		b.WriteString(postgresCopyValue(c.Username) + "\t" + postgresCopyValue(c.Database) + "\t" + postgresCopyValue(secret.ClientPasswords[c.AppID]) + "\n")
	}
	b.WriteString("\\.\nCREATE TEMP TABLE s2h_admin(password text NOT NULL);\nCOPY s2h_admin FROM STDIN;\n" + postgresCopyValue(secret.AdminPassword) + "\n\\.\n")
	b.WriteString("SELECT format('CREATE ROLE %I SUPERUSER CREATEDB CREATEROLE LOGIN INHERIT NOREPLICATION NOBYPASSRLS', 's2h_admin') WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='s2h_admin') \\gexec\nSELECT format('ALTER ROLE %I SUPERUSER CREATEDB CREATEROLE LOGIN INHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L', 's2h_admin', password) FROM s2h_admin \\gexec\n")
	for _, db := range postgresCatalogProtocolDatabases(append(append([]hostcontract.LocalDataClient{}, e.Desired...), e.Previous...)) {
		owner := postgresOwner(e.Binding.Service, db)
		b.WriteString("SELECT format('CREATE ROLE %I NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', " + sqlQuote(owner) + ") WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=" + sqlQuote(owner) + ") \\gexec\nSELECT format('ALTER ROLE %I NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', " + sqlQuote(owner) + ") \\gexec\n")
		marker := postgresCatalogProtocolOwnerMarker(e, db)
		if !containsPostgresCatalogProtocolDatabase(postgresCatalogProtocolDatabases(e.Desired), db) {
			marker = postgresCatalogProtocolOwnerMarker(postgresCatalogProtocolPreviousExpected(e), db)
		}
		b.WriteString("SELECT format('COMMENT ON ROLE %I IS %L', " + sqlQuote(owner) + ", " + sqlQuote(marker) + ") \\gexec\n")
	}
	for _, db := range postgresCatalogProtocolDatabases(e.Desired) {
		creator := postgresCatalogProtocolCreator(e, db)
		b.WriteString("SELECT format('CREATE ROLE %I NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', " + sqlQuote(creator) + ") WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=" + sqlQuote(creator) + ") \\gexec\nSELECT format('ALTER ROLE %I NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', " + sqlQuote(creator) + ") \\gexec\nSELECT format('COMMENT ON ROLE %I IS %L', " + sqlQuote(creator) + ", " + sqlQuote(postgresCatalogProtocolCreatorMarker(e, db)) + ") \\gexec\n")
	}
	prior := postgresCatalogProtocolPreviousExpected(e)
	for _, db := range postgresCatalogProtocolDatabases(e.Previous) {
		creator := postgresCatalogProtocolCreator(prior, db)
		b.WriteString("SELECT format('CREATE ROLE %I NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', " + sqlQuote(creator) + ") WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=" + sqlQuote(creator) + ") \\gexec\nSELECT format('ALTER ROLE %I NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', " + sqlQuote(creator) + ") \\gexec\nSELECT format('COMMENT ON ROLE %I IS %L', " + sqlQuote(creator) + ", " + sqlQuote(postgresCatalogProtocolCreatorMarker(prior, db)) + ") \\gexec\n")
	}
	for _, user := range postgresCatalogProtocolUsernames(e.Desired) {
		b.WriteString("SELECT format('CREATE ROLE %I LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L', username, password) FROM (SELECT DISTINCT username, password FROM s2h_clients WHERE username=" + sqlQuote(user) + ") c WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=c.username) \\gexec\nSELECT format('ALTER ROLE %I LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L', username, password) FROM (SELECT DISTINCT username, password FROM s2h_clients WHERE username=" + sqlQuote(user) + ") c \\gexec\nSELECT format('COMMENT ON ROLE %I IS %L', " + sqlQuote(user) + ", " + sqlQuote(postgresCatalogProtocolClientMarker(e, user)) + ") \\gexec\n")
	}
	for _, c := range e.Desired {
		owner := postgresOwner(e.Binding.Service, c.Database)
		b.WriteString("SELECT format('GRANT %I TO %I WITH INHERIT FALSE SET TRUE', " + sqlQuote(owner) + ", " + sqlQuote(c.Username) + ") \\gexec\n")
	}
	for _, c := range e.Previous {
		if !containsPostgresCatalogProtocolDatabase(postgresCatalogProtocolUserDatabases(e.Desired, c.Username), c.Database) {
			b.WriteString("SELECT format('REVOKE %I FROM %I', " + sqlQuote(postgresOwner(e.Binding.Service, c.Database)) + ", " + sqlQuote(c.Username) + ") \\gexec\nSELECT format('ALTER ROLE %I IN DATABASE %I RESET ROLE', " + sqlQuote(c.Username) + ", " + sqlQuote(c.Database) + ") \\gexec\n")
		}
	}
	for _, user := range postgresCatalogProtocolPreviousOnlyUsers(e) {
		b.WriteString("SELECT format('ALTER ROLE %I NOLOGIN', " + sqlQuote(user) + ") \\gexec\nSELECT format('COMMENT ON ROLE %I IS %L', " + sqlQuote(user) + ", " + sqlQuote(postgresCatalogProtocolClientMarker(prior, user)) + ") \\gexec\n")
	}
	b.WriteString("SELECT format('COMMENT ON ROLE %I IS %L', 's2h_admin', " + sqlQuote(postgresCatalogProtocolRolesMarker(e)) + ") \\gexec\nCOMMIT;\n")
	return b.String(), nil
}

func postgresProtocolCreateSQL(e postgresCatalogProtocolExpected, db string) string {
	return "SELECT format('CREATE DATABASE %I OWNER %I', " + sqlQuote(db) + ", " + sqlQuote(postgresCatalogProtocolCreator(e, db)) + ") WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname=" + sqlQuote(db) + ") \\gexec\n"
}
func postgresProtocolFinalizeSQL(e postgresCatalogProtocolExpected, db string) string {
	owner := postgresOwner(e.Binding.Service, db)
	var b strings.Builder
	b.WriteString("BEGIN;\nSELECT format('ALTER DATABASE %I OWNER TO %I', " + sqlQuote(db) + ", " + sqlQuote(owner) + ") \\gexec\nSELECT format('ALTER SCHEMA public OWNER TO %I', " + sqlQuote(owner) + ") \\gexec\nREVOKE CREATE ON SCHEMA public FROM PUBLIC;\nSELECT format('GRANT USAGE, CREATE ON SCHEMA public TO %I', " + sqlQuote(owner) + ") \\gexec\nSELECT format('GRANT CONNECT ON DATABASE %I TO %I', " + sqlQuote(db) + ", " + sqlQuote(owner) + ") \\gexec\nSELECT format('ALTER ROLE %I IN DATABASE %I RESET ROLE', " + sqlQuote(owner) + ", " + sqlQuote(db) + ") \\gexec\n")
	for _, user := range postgresCatalogProtocolUsernames(append(append([]hostcontract.LocalDataClient{}, e.Desired...), e.Previous...)) {
		b.WriteString("SELECT format('REVOKE CONNECT ON DATABASE %I FROM %I', " + sqlQuote(db) + ", " + sqlQuote(user) + ") \\gexec\nSELECT format('REVOKE ALL ON SCHEMA public FROM %I', " + sqlQuote(user) + ") \\gexec\nSELECT format('ALTER ROLE %I IN DATABASE %I RESET ROLE', " + sqlQuote(user) + ", " + sqlQuote(db) + ") \\gexec\n")
	}
	for _, user := range postgresCatalogProtocolUsernames(e.Desired) {
		if containsPostgresCatalogProtocolDatabase(postgresCatalogProtocolUserDatabases(e.Desired, user), db) {
			b.WriteString("SELECT format('GRANT CONNECT ON DATABASE %I TO %I', " + sqlQuote(db) + ", " + sqlQuote(user) + ") \\gexec\nSELECT format('ALTER ROLE %I IN DATABASE %I SET ROLE TO %I', " + sqlQuote(user) + ", " + sqlQuote(db) + ", " + sqlQuote(owner) + ") \\gexec\n")
		}
	}
	b.WriteString("SELECT format('COMMENT ON DATABASE %I IS %L', " + sqlQuote(db) + ", " + sqlQuote(postgresCatalogProtocolDatabaseMarker(e, db)) + ") \\gexec\nCOMMIT;\n")
	return b.String()
}

func (r *Runtime) reconcilePostgres(ctx context.Context, s State, o managedObject, e postgresCatalogProtocolExpected, secret hostcontract.LocalDataServiceSecrets) error {
	classification, err := r.postgresClassification(ctx, s, o, e)
	if err != nil {
		return recovery()
	}
	if classification.State == postgresCatalogProtocolForeign {
		return conflict()
	}
	if classification.State == postgresCatalogProtocolUnavailable || classification.State == postgresCatalogProtocolMixed {
		return recovery()
	}
	if classification.State == postgresCatalogProtocolPrior {
		sql, err := postgresProtocolRoleSQL(e, secret)
		if err != nil {
			return operationFailed()
		}
		if _, err = r.runner.Run(ctx, postgresPSQLArgs(o.Name), []byte(sql)); err != nil {
			after, observeErr := r.postgresClassification(ctx, s, o, e)
			if observeErr != nil || after.State != postgresCatalogProtocolPartial && after.State != postgresCatalogProtocolExact {
				return operationFailed()
			}
			return operationFailed()
		}
	}
	for _, db := range postgresCatalogProtocolDatabases(e.Desired) {
		classification, err = r.postgresClassification(ctx, s, o, e)
		if err != nil {
			return recovery()
		}
		if classification.State == postgresCatalogProtocolExact {
			return nil
		}
		if classification.State == postgresCatalogProtocolForeign {
			return conflict()
		}
		if classification.State != postgresCatalogProtocolPartial {
			return recovery()
		}
		roles, dbs, observeErr := r.observePostgresCatalog(ctx, o, e)
		if observeErr != nil || roles.Marker != postgresCatalogProtocolTarget || dbs == nil {
			return recovery()
		}
		var current postgresCatalogProtocolDatabaseObservation
		for _, observed := range dbs {
			if observed.Key == postgresCatalogProtocolDatabaseToken(db) {
				current = observed
				break
			}
		}
		if current.Reference.State == postgresCatalogProtocolAbsent {
			if _, err = r.runner.Run(ctx, postgresPSQLArgs(o.Name), []byte(postgresProtocolCreateSQL(e, db))); err != nil {
				after, observeErr := r.postgresClassification(ctx, s, o, e)
				if observeErr != nil || after.State != postgresCatalogProtocolPartial {
					return operationFailed()
				}
				return operationFailed()
			}
		}
		if _, err = r.runner.Run(ctx, append(postgresPSQLArgs(o.Name), "-d", db), []byte(postgresProtocolFinalizeSQL(e, db))); err != nil {
			after, observeErr := r.postgresClassification(ctx, s, o, e)
			if observeErr != nil || after.State != postgresCatalogProtocolPartial && after.State != postgresCatalogProtocolExact {
				return operationFailed()
			}
			return operationFailed()
		}
	}
	classification, err = r.postgresClassification(ctx, s, o, e)
	if err != nil || classification.State != postgresCatalogProtocolExact {
		return recovery()
	}
	return nil
}
func (r *Runtime) reconcilePostgresAdmin(ctx context.Context, o managedObject, password string) error {
	sql, err := postgresPasswordSQL(password)
	if err != nil {
		return operationFailed()
	}
	if _, err = r.runner.Run(ctx, []string{"exec", "-i", o.Name, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "s2h_admin", "-d", "postgres"}, sql); err != nil {
		return operationFailed()
	}
	return nil
}
func (r *Runtime) postgresPasswordChanged(old managedObject, password string) (bool, error) {
	b, err := r.readArtifactBytes(old.Env)
	if err != nil {
		return false, err
	}
	if !strings.HasSuffix(string(b), "\n") || strings.ContainsAny(string(b[:len(b)-1]), "\r\n") {
		return false, errors.New("malformed postgres env")
	}
	return string(b[:len(b)-1]) != password, nil
}
func postgresPasswordSQL(password string) ([]byte, error) {
	if !validLocalPassword(password) {
		return nil, errors.New("invalid postgres password")
	}
	return []byte("CREATE TEMP TABLE s2h_password(password text NOT NULL);\nCOPY s2h_password FROM STDIN;\n" + postgresCopyValue(password) + "\n\\.\nSELECT format('ALTER ROLE %I PASSWORD %L', 's2h_admin', password) FROM s2h_password \\gexec\n"), nil
}
func (r *Runtime) runLocal(ctx context.Context, s State, o managedObject, target hostcontract.LocalDataServiceTarget) error {
	label := ownershipLabelFor(s.Resource, s.Ownership, o.Role, o.AppToken, "")
	args := []string{"run", "-d", "--restart", "unless-stopped", "--label", "sub2api.host=" + label, "--label", "sub2api.host.target=" + targetLabelFor(o), "--name", o.Name, "--network", networkName(s), "--network-alias", target.ID}
	for _, binding := range target.Bindings {
		address := binding.Address
		if strings.Contains(address, ":") {
			address = "[" + address + "]"
		}
		args = append(args, "-p", address+":"+strconv.Itoa(target.Port)+":"+strconv.Itoa(target.Port)+"/tcp")
	}
	args = append(args, "-v", r.dataPath(o.DataToken)+":")
	if o.Type == "postgres" {
		args[len(args)-1] += "/var/lib/postgresql/data"
		args = append(args, "-v", r.artifactPath(o.Env)+":/run/secrets/postgres-admin:ro", "-v", r.artifactPath(o.Config)+":/etc/sub2api/postgresql.conf:ro", "-v", r.artifactPath(o.HBA)+":/etc/sub2api/pg_hba.conf:ro", "-v", r.artifactPath(o.Ident)+":/etc/sub2api/pg_ident.conf:ro", "-e", "POSTGRES_USER=s2h_admin", "-e", "POSTGRES_DB=postgres", "-e", "POSTGRES_PASSWORD_FILE=/run/secrets/postgres-admin", "-e", "POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256 --auth-local=peer", o.Image, "-c", "config_file=/etc/sub2api/postgresql.conf", "-p", strconv.Itoa(o.Port))
	} else {
		args[len(args)-1] += "/data"
		args = append(args, "--env-file", r.artifactPath(o.Env), "-v", r.artifactPath(o.Config)+":/usr/local/etc/redis/redis.conf:ro", o.Image, "redis-server", "/usr/local/etc/redis/redis.conf")
	}
	return r.docker(ctx, args...)
}
func traefikStaticConfig(email string) []byte {
	return []byte("entryPoints:\n  web:\n    address: \":80\"\n  websecure:\n    address: \":443\"\n  probe:\n    address: \":8081\"\nproviders:\n  file:\n    directory: /etc/traefik/dynamic\n    watch: true\ncertificatesResolvers:\n  cloudflare:\n    acme:\n      email: " + yamlQuote(email) + "\n      storage: /etc/traefik/acme.json\n      dnsChallenge:\n        provider: cloudflare\n")
}
func (r *Runtime) runProxy(ctx context.Context, s State, o managedObject) error {
	return r.docker(ctx, "run", "-d", "--restart", "unless-stopped", "--label", "sub2api.host="+ownershipLabelFor(s.Resource, s.Ownership, "proxy", "", ""), "--label", "sub2api.host.target="+targetLabelFor(o), "--name", o.Name, "--network", networkName(s), "--env-file", r.artifactPath(o.Env), "-p", "80:80", "-p", "443:443", "-v", r.artifactPath(o.Config)+":/etc/traefik/traefik.yml:ro", "-v", r.dynamicPath()+":/etc/traefik/dynamic:ro", "-v", r.proxyACMEPath()+":/etc/traefik/acme.json", o.Image)
}
func (r *Runtime) proxyReady(ctx context.Context, o managedObject) error {
	return r.docker(ctx, "exec", o.Name, "traefik", "version")
}
func (r *Runtime) inspectCandidate(ctx context.Context, inv inventory, o managedObject) error {
	_, err := r.ownedPresent(ctx, inv, o)
	return err
}
func (r *Runtime) candidateExists(ctx context.Context, inv inventory, o managedObject) (bool, error) {
	return r.ownedPresent(ctx, inv, o)
}
func appToken(id string) string      { return token("app", id) }
func appDataToken(app string) string { return token("app-data", app) }
func objectName(s State, role, app, slot string) string {
	return "s2h-" + token(s.Resource.Environment, s.Resource.ServerKey, s.Ownership.Value, role, app, slot)
}
func ownershipLabelFor(resource hostcontract.ResourceIdentity, owner hostcontract.OwnershipIdentity, role, app, slot string) string {
	return "s2h1:" + token(resource.Environment, resource.ServerKey, owner.Value, role, app, slot)
}
func targetLabelFor(o managedObject) string {
	return "s2ht1:" + token(o.Role, o.AppToken, o.Active, o.Revision, o.Image, o.Type, strconv.Itoa(o.Port), strconv.FormatBool(o.Persistence))
}
func token(values ...string) string {
	h := sha256.New()
	for _, v := range values {
		_, _ = h.Write([]byte(v))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}
func operationFailed() error {
	return &RemoteError{Category: hostprotocol.ErrorRemoteOperation, Code: hostprotocol.CodeOperationFailed}
}
func isConflict(err error) bool {
	remote, ok := err.(*RemoteError)
	return ok && remote.Category == hostprotocol.ErrorConflict && remote.Code == hostprotocol.CodeOperationConflict
}
func approvalRequired() error {
	return &RemoteError{Category: hostprotocol.ErrorApproval, Code: hostprotocol.CodeApprovalRequired}
}

func (r *Runtime) artifactPath(name string) string {
	return filepath.Join(append(append([]string{r.root, "runtime"}, artifactParts(artifactDirectory(name))...), name)...)
}
func validArtifactName(name string) bool {
	return name == artifactInventory || (strings.HasPrefix(name, artifactEnvPrefix) && len(name) == len(artifactEnvPrefix)+48 && lowerHex(strings.TrimPrefix(name, artifactEnvPrefix))) || (strings.HasPrefix(name, artifactConfigPrefix) && len(name) == len(artifactConfigPrefix)+24 && lowerHex(strings.TrimPrefix(name, artifactConfigPrefix))) || (strings.HasPrefix(name, artifactRoutePrefix) && len(name) == len(artifactRoutePrefix)+24+len(".json") && strings.HasSuffix(name, ".json") && lowerHex(strings.TrimSuffix(strings.TrimPrefix(name, artifactRoutePrefix), ".json")))
}
func (r *Runtime) dataPath(token string) string {
	return filepath.Join(r.root, "runtime", "data", token)
}
func (r *Runtime) dynamicPath() string { return filepath.Join(r.root, "runtime", "dynamic") }
func (r *Runtime) proxyACMEPath() string {
	return filepath.Join(r.root, "runtime", "proxy", "acme", "acme.json")
}
func (r *Runtime) ensureDataDir(token string) error { return r.ensureRuntimeDir("data", token) }
func (r *Runtime) ensureProxyPaths() error {
	if err := r.ensureRuntimeDir("dynamic"); err != nil {
		return err
	}
	if err := r.ensureRuntimeDir("proxy", "acme"); err != nil {
		return err
	}
	dir, err := r.runtimeDir(false, "proxy", "acme")
	if err != nil {
		return err
	}
	defer syscall.Close(dir)
	fd, err := syscall.Openat(dir, "acme.json", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if errors.Is(err, syscall.EEXIST) {
		fd, err = syscall.Openat(dir, "acme.json", syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	}
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Mode&0077 != 0 || int(st.Uid) != r.expectedUID || st.Nlink != 1 || st.Size > maxArtifactSize {
		return errors.New("unsafe acme")
	}
	if err := syscall.Fsync(fd); err != nil {
		return err
	}
	return syscall.Fsync(dir)
}
func (r *Runtime) validateACMEPath() error {
	dir, err := r.runtimeDir(false, "proxy", "acme")
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer syscall.Close(dir)
	fd, err := syscall.Openat(dir, "acme.json", syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Mode&0077 != 0 || int(st.Uid) != r.expectedUID || st.Nlink != 1 || st.Size > maxArtifactSize {
		return errors.New("unsafe acme")
	}
	return nil
}
func (r *Runtime) ensureRuntimeDir(parts ...string) error {
	fd, err := r.runtimeDir(true, parts...)
	if err == nil {
		err = syscall.Close(fd)
	}
	return err
}
func (r *Runtime) writeInventory(v inventory) error {
	if validateInventory(v) != nil {
		return errors.New("invalid inventory")
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return r.writeArtifact(artifactInventory, b, 0600)
}
func (r *Runtime) readInventory() (inventory, error) {
	b, e := r.readArtifactBytes(artifactInventory)
	if e != nil {
		return inventory{}, e
	}
	var v inventory
	if strictJSON(b, &v) != nil || validateInventory(v) != nil {
		return inventory{}, errors.New("invalid inventory")
	}
	return v, nil
}
func validateInventory(v inventory) error {
	if _, err := hostcontract.ParseRevision(v.AppliedRevision); v.Version != inventoryVersion || err != nil || v.Resource.Environment == "" || v.Resource.ServerKey == "" || v.Ownership.Value == "" || !utf8.ValidString(v.Resource.Environment) || !utf8.ValidString(v.Resource.ServerKey) || !utf8.ValidString(v.Ownership.Value) {
		return errors.New("inventory")
	}
	objects, names, logical := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, o := range v.Objects {
		if o.Role != "app" && o.Role != "app-data" && o.Role != "local-data" && o.Role != "local-data-meta" && o.Role != "proxy" || (o.Role != "proxy" && (len(o.AppToken) != 24 || !lowerHex(o.AppToken))) || objects[o.Role+"\x00"+o.AppToken] {
			return errors.New("inventory object")
		}
		objects[o.Role+"\x00"+o.AppToken] = true
		if o.Role == "local-data" || o.Role == "local-data-meta" {
			if logical[o.AppToken] {
				return errors.New("inventory local duplicate")
			}
			logical[o.AppToken] = true
		}
		links := map[string]bool{}
		for _, link := range o.Data {
			if link.Name == "" || !utf8.ValidString(link.Name) || links[link.Name] || !validDataIdentity(link.Identity) {
				return errors.New("inventory data")
			}
			links[link.Name] = true
		}
		if o.Role == "app" {
			if o.Name == "" || o.Image == "" || o.Revision == "" || o.Hostname == "" || o.ReadinessPath == "" || o.DrainSeconds < 1 || o.DrainSeconds > 600 || (o.Active != "blue" && o.Active != "green") || names[o.Name] || !utf8.ValidString(o.Name) || !utf8.ValidString(o.Image) || !validHostname(o.Hostname) || o.Type != "" || o.Port != 0 || o.Persistence || o.DataToken != "" || o.PathToken != "" || o.DataIdentity != (hostcontract.DataIdentity{}) || o.Config != "" {
				return errors.New("inventory app")
			}
			if _, err := hostcontract.ParseRevision(o.Revision); err != nil {
				return errors.New("inventory app")
			}
			if o.Name != objectName(State{Resource: v.Resource, Ownership: v.Ownership}, "app", o.AppToken, o.Active) || o.Env != envName(o.AppToken, o.Revision) {
				return errors.New("inventory app")
			}
			names[o.Name] = true
		} else if o.Role == "app-data" && (o.Name != "" || o.Image != "" || o.Revision != "" || o.Active != "" || o.Env != "" || len(o.Data) == 0 || o.Type != "" || o.Port != 0 || o.Persistence || o.DataToken != "" || o.PathToken != "" || o.DataIdentity != (hostcontract.DataIdentity{}) || o.Config != "" || o.Hostname != "" || o.ReadinessPath != "" || o.DrainSeconds != 0) {
			return errors.New("inventory data")
		} else if o.Role == "local-data" {
			if o.Name == "" || names[o.Name] || !validClientAppID(o.Service) || (o.Type != "postgres" && o.Type != "redis") || (o.Type == "postgres" && o.Image != postgresImage) || (o.Type == "redis" && o.Image != redisImage) || o.Port < 1 || o.Port > 65535 || o.Revision == "" || len(o.DataToken) != 24 || !lowerHex(o.DataToken) || len(o.PathToken) != 24 || !lowerHex(o.PathToken) || o.DataToken != token("data", o.AppToken) || o.PathToken != token("path", o.AppToken) || !validDataIdentity(o.DataIdentity) || len(o.Data) != 0 || o.Active != "" || o.Hostname != "" || o.ReadinessPath != "" || o.DrainSeconds != 0 {
				return errors.New("inventory local data")
			}
			database, tls := "sub2api", o.Name
			if o.Type == "redis" {
				database, tls = "0", ""
			}
			expectedIdentity := hostcontract.DataIdentity{Kind: o.Type, ProviderID: o.Name, Endpoint: o.Name, Port: o.Port, Database: database, TLSServerName: tls}
			if _, err := hostcontract.ParseRevision(o.Revision); err != nil || o.DataIdentity != expectedIdentity {
				return errors.New("inventory local identity")
			}
			expectedConfig := ""
			if o.Type == "redis" {
				expectedConfig = artifactConfigPrefix + token(o.AppToken, o.Revision)
			}
			expectedHBA := ""
			if o.Type == "postgres" {
				expectedConfig = artifactConfigPrefix + token(o.AppToken, o.Revision, "postgresql")
				expectedHBA = artifactConfigPrefix + token(o.AppToken, o.Revision, "hba")
			}
			expectedIdent := ""
			if o.Type == "postgres" {
				expectedIdent = artifactConfigPrefix + token(o.AppToken, o.Revision, "ident")
			}
			if o.Name != objectName(State{Resource: v.Resource, Ownership: v.Ownership}, "local-data", o.AppToken, "live") || o.Env != envName(o.AppToken, o.Revision) || o.Config != expectedConfig || o.HBA != expectedHBA || o.Ident != expectedIdent {
				return errors.New("inventory local data")
			}
			bindings, clients := map[string]bool{}, map[string]bool{}
			for _, binding := range o.Bindings {
				if !validInventoryBinding(binding) || bindings[binding.Address] {
					return errors.New("inventory local binding")
				}
				bindings[binding.Address] = true
			}
			for _, client := range o.Clients {
				validDatabase := validPrincipal(client.Database)
				validUsername := validPrincipal(client.Username)
				if o.Type == "redis" {
					validDatabase = validRedisDatabase(client.Database)
					validUsername = validUsername && client.Username != "default"
				}
				if !validClientAppID(client.AppID) || !validUsername || !validDatabase || clients[client.AppID] {
					return errors.New("inventory local client")
				}
				clients[client.AppID] = true
			}
			names[o.Name] = true
		} else if o.Role == "local-data-meta" {
			if o.Name != "" || o.Type == "" || o.DataIdentity.Kind != o.Type || len(o.DataToken) != 24 || !lowerHex(o.DataToken) || len(o.PathToken) != 24 || !lowerHex(o.PathToken) || o.DataToken != token("data", o.AppToken) || o.PathToken != token("path", o.AppToken) || !validDataIdentity(o.DataIdentity) || o.Image != "" || o.Revision != "" || o.Active != "" || o.Env != "" || o.Config != "" || o.HBA != "" || o.Ident != "" || len(o.Data) != 0 || o.Port != 0 || o.Persistence || o.Hostname != "" || o.ReadinessPath != "" || o.DrainSeconds != 0 {
				return errors.New("inventory local data meta")
			}
		} else if o.Role == "proxy" {
			if o.Name == "" || names[o.Name] || o.Image == "" || o.Revision == "" || !validArtifactName(o.Env) || !validArtifactName(o.Config) || o.AppToken != "" || o.Active != "" || len(o.Data) != 0 || o.Type != "" || o.Port != 0 || o.Persistence || o.DataToken != "" || o.PathToken != "" || o.DataIdentity != (hostcontract.DataIdentity{}) || o.Hostname != "" || o.ReadinessPath != "" || o.DrainSeconds != 0 {
				return errors.New("inventory proxy")
			}
			if _, err := hostcontract.ParseRevision(o.Revision); err != nil {
				return errors.New("inventory proxy")
			}
			proxyToken := token("proxy")
			if o.Name != objectName(State{Resource: v.Resource, Ownership: v.Ownership}, "proxy", "proxy", "live") || o.Env != envName(proxyToken, o.Revision) || o.Config != artifactConfigPrefix+token("proxy", o.Revision) {
				return errors.New("inventory proxy")
			}
			names[o.Name] = true
		}
	}
	return nil
}
func validInventoryBinding(binding hostcontract.LocalDataBinding) bool {
	if net.ParseIP(binding.Address) == nil || len(binding.AllowedSources) == 0 {
		return false
	}
	for _, source := range binding.AllowedSources {
		if net.ParseIP(source) == nil {
			return false
		}
	}
	return true
}
func validClientAppID(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	for _, c := range value {
		if !(c == '-' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func validDataIdentity(d hostcontract.DataIdentity) bool {
	if !utf8.ValidString(d.Kind) || !utf8.ValidString(d.ProviderID) || !utf8.ValidString(d.Endpoint) || !utf8.ValidString(d.Database) || !utf8.ValidString(d.TLSServerName) {
		return false
	}
	return hostcontract.ValidateTarget(hostcontract.Target{ReleaseArtifact: "inventory", Apps: []hostcontract.AppTarget{{ID: "app", Image: "inventory", Hostname: "inventory", ReadinessPath: "/", InitialAdminEmail: "admin@example.test", DataLinks: []hostcontract.DataLink{{Name: "link", Identity: d}}}}}, hostcontract.Secrets{}) == nil
}
func safeSecrets(s hostcontract.AppSecrets) bool {
	values := []string{s.InitialAdminPassword, s.JWTSecret, s.TOTPEncryptionKey, s.AdminAPIKey}
	if s.Postgres != nil {
		values = append(values, s.Postgres.Username, s.Postgres.Password)
	}
	if s.Redis != nil {
		values = append(values, s.Redis.Username, s.Redis.Password)
	}
	for k, v := range s.RuntimeEnvironment {
		values = append(values, k, v)
	}
	for _, v := range values {
		if !safeEnvValue(v) {
			return false
		}
	}
	return true
}
func safeEnvironment(a hostcontract.AppTarget, s hostcontract.AppSecrets) bool {
	if !safeSecrets(s) {
		return false
	}
	reserved := map[string]bool{"ADMIN_EMAIL": true, "INITIAL_ADMIN_PASSWORD": true, "JWT_SECRET": true, "TOTP_ENCRYPTION_KEY": true, "ADMIN_API_KEY": true, "DATABASE_HOST": true, "DATABASE_PORT": true, "DATABASE_USER": true, "DATABASE_PASSWORD": true, "DATABASE_DBNAME": true, "DATABASE_SSLMODE": true, "REDIS_HOST": true, "REDIS_PORT": true, "REDIS_USERNAME": true, "REDIS_PASSWORD": true, "REDIS_DB": true, "REDIS_ENABLE_TLS": true}
	for k, v := range a.RuntimeSettings {
		if reserved[k] || !validEnvKey(k) || !safeEnvValue(v) {
			return false
		}
	}
	for k, v := range s.RuntimeEnvironment {
		if reserved[k] || !validEnvKey(k) || !safeEnvValue(v) {
			return false
		}
	}
	for k := range a.RuntimeSettings {
		if _, ok := s.RuntimeEnvironment[k]; ok {
			return false
		}
	}
	return true
}
func safeEnvValue(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, c := range value {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
func validAppDataLinks(a hostcontract.AppTarget, s hostcontract.AppSecrets) bool {
	wanted := map[string]bool{}
	for _, link := range a.DataLinks {
		identity := link.Identity
		if (identity.Kind != "postgres" && identity.Kind != "redis") || wanted[identity.Kind] || !validDataIdentity(identity) || !safeEnvValue(identity.Endpoint) || !safeEnvValue(identity.Database) || !safeEnvValue(identity.TLSMode) {
			return false
		}
		wanted[identity.Kind] = true
	}
	if wanted["postgres"] != (s.Postgres != nil) || wanted["redis"] != (s.Redis != nil) {
		return false
	}
	for _, credentials := range []*hostcontract.DataCredentials{s.Postgres, s.Redis} {
		if credentials != nil && (!validPrincipal(credentials.Username) || !validLocalPassword(credentials.Password)) {
			return false
		}
	}
	return true
}
func validLocalDataCredentials(target hostcontract.LocalDataServiceTarget, secret hostcontract.LocalDataServiceSecrets) bool {
	seen := map[string]bool{}
	for _, client := range target.Clients {
		validClient := validPostgresClient(client)
		if target.Type == "redis" {
			validClient = validClientAppID(client.AppID) && validPrincipal(client.Username) && client.Username != "default" && validRedisDatabase(client.Database)
		}
		if seen[client.AppID] || !validClient || !validLocalPassword(secret.ClientPasswords[client.AppID]) {
			return false
		}
		seen[client.AppID] = true
	}
	if len(seen) != len(secret.ClientPasswords) {
		return false
	}
	for appID := range secret.ClientPasswords {
		if !seen[appID] {
			return false
		}
	}
	return true
}
func validRedisDatabase(value string) bool {
	if value == "" || len(value) > 2 {
		return false
	}
	parsed, err := strconv.Atoi(value)
	return err == nil && parsed >= 0 && parsed <= 15 && strconv.Itoa(parsed) == value
}
func validEnvKey(v string) bool {
	if v == "" {
		return false
	}
	for i, c := range v {
		if !(c == '_' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func routeName(token string) string { return artifactRoutePrefix + token + ".json" }
func (r *Runtime) routeBytes(token string) ([]byte, bool) {
	b, err := r.readArtifactBytes(routeName(token))
	return b, err == nil
}
func (r *Runtime) routeMatches(inv inventory, o managedObject) bool {
	b, err := r.readArtifactBytes(routeName(o.AppToken))
	if err != nil {
		return false
	}
	expected, _ := routeBytesFor(inv, o)
	return bytes.Equal(b, expected)
}
func (r *Runtime) writeRoute(inv inventory, o managedObject) error {
	b, err := routeBytesFor(inv, o)
	if err == nil {
		err = r.writeArtifact(routeName(o.AppToken), b, 0600)
	}
	if err == nil && routeWriteHook != nil {
		err = routeWriteHook()
	}
	return err
}
func routeBytesFor(inv inventory, o managedObject) ([]byte, error) {
	key := token("route", inv.Resource.Environment, inv.Resource.ServerKey, inv.Ownership.Value, o.AppToken, o.Revision, o.Active, o.Name)
	service := token("service", key)
	var route traefikRoute
	publicKey := token("router", "public", key)
	probeKey := token("router", "probe", key)
	route.HTTP.Routers = map[string]traefikRouter{
		publicKey: {Rule: "Host(`" + hostnameFor(inv, o) + "`)", EntryPoints: []string{"websecure"}, Service: service, TLS: &traefikTLS{CertResolver: "cloudflare"}},
		probeKey:  {Rule: "Host(`" + hostnameFor(inv, o) + "`)", EntryPoints: []string{"probe"}, Service: service},
	}
	serviceValue := traefikService{}
	serviceValue.LoadBalancer.Servers = []traefikServer{{URL: "http://" + o.Name + ":8080"}}
	route.HTTP.Services = map[string]traefikService{service: serviceValue}
	return json.Marshal(route)
}
func hostnameFor(_ inventory, o managedObject) string { return o.Hostname }
func (r *Runtime) restoreRoute(token string, oldRoute []byte, existed bool) error {
	var err error
	if existed {
		err = r.writeArtifact(routeName(token), oldRoute, 0600)
	} else {
		err = r.removeArtifact(routeName(token))
	}
	if err == nil && routeRestoreHook != nil {
		err = routeRestoreHook()
	}
	return err
}
func (r *Runtime) removeRoute(inv inventory, o managedObject) error {
	if !r.routeMatches(inv, o) {
		return recovery()
	}
	return r.removeArtifact(routeName(o.AppToken))
}
func (r *Runtime) removeRouteProgress(inv inventory, o managedObject, allowAbsent bool) error {
	if r.routeMatches(inv, o) {
		if err := r.removeArtifact(routeName(o.AppToken)); err != nil {
			return err
		}
		if routeRemoveHook != nil {
			return routeRemoveHook(o.AppToken)
		}
		return nil
	}
	if allowAbsent {
		if _, err := r.readArtifactBytes(routeName(o.AppToken)); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	return recovery()
}
func (r *Runtime) removeArtifact(name string) error {
	if !validArtifactName(name) {
		return errors.New("artifact")
	}
	dir, err := r.runtimeDir(false, artifactParts(artifactDirectory(name))...)
	if err != nil {
		return err
	}
	defer syscall.Close(dir)
	fd, err := syscall.Openat(dir, name, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Mode&0077 != 0 || int(st.Uid) != r.expectedUID || st.Nlink != 1 {
		syscall.Close(fd)
		return errors.New("unsafe artifact")
	}
	syscall.Close(fd)
	if err = syscall.Unlinkat(dir, name); err != nil {
		return err
	}
	if err = syscall.Fsync(dir); err != nil {
		return err
	}
	if artifactRemoveHook != nil {
		return artifactRemoveHook(name)
	}
	return nil
}
func (r *Runtime) removeEnv(name string) error {
	err := r.removeArtifact(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type artifactDir uint8

const (
	managedDir artifactDir = iota
	dynamicDir
)

func artifactDirectory(name string) artifactDir {
	if strings.HasPrefix(name, artifactRoutePrefix) {
		return dynamicDir
	}
	return managedDir
}
func (r *Runtime) writeArtifact(name string, b []byte, mode uint32) error {
	if !validArtifactName(name) || mode != 0600 || len(b) > maxArtifactSize {
		return errors.New("artifact")
	}
	dir, e := r.runtimeDir(true, artifactParts(artifactDirectory(name))...)
	if e != nil {
		return e
	}
	defer syscall.Close(dir)
	var st syscall.Stat_t
	if syscall.Fstat(dir, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || st.Mode&0077 != 0 || int(st.Uid) != r.expectedUID {
		return errors.New("unsafe artifact directory")
	}
	for i := 0; i < 16; i++ {
		tmp := ".tmp-" + token(name, string(rune('a'+i)))
		fd, x := syscall.Openat(dir, tmp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if x == syscall.EEXIST {
			continue
		}
		if x != nil {
			return x
		}
		x = syscall.Fchmod(fd, 0600)
		for off := 0; x == nil && off < len(b); {
			n, w := syscall.Write(fd, b[off:])
			if w != nil {
				x = w
			} else if n == 0 {
				x = io.ErrShortWrite
			}
			off += n
		}
		if x == nil {
			x = syscall.Fsync(fd)
		}
		if c := syscall.Close(fd); x == nil {
			x = c
		}
		if x == nil {
			x = syscall.Renameat(dir, tmp, dir, name)
		}
		_ = syscall.Unlinkat(dir, tmp)
		if x == nil {
			x = syscall.Fsync(dir)
		}
		return x
	}
	return errors.New("artifact collision")
}
func (r *Runtime) readArtifactBytes(name string) ([]byte, error) {
	if !validArtifactName(name) {
		return nil, errors.New("artifact")
	}
	dir, e := r.runtimeDir(false, artifactParts(artifactDirectory(name))...)
	if e != nil {
		return nil, e
	}
	defer syscall.Close(dir)
	fd, e := syscall.Openat(dir, name, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Mode&0077 != 0 || int(st.Uid) != r.expectedUID || st.Nlink != 1 || st.Size > maxArtifactSize {
		syscall.Close(fd)
		return nil, errors.New("unsafe artifact")
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxArtifactSize+1))
}

func artifactParts(dir artifactDir) []string {
	if dir == dynamicDir {
		return []string{"dynamic"}
	}
	return []string{"managed"}
}
func (r *Runtime) runtimeDir(create bool, parts ...string) (int, error) {
	root, err := r.rootFD(create)
	if err != nil {
		return -1, err
	}
	defer syscall.Close(root)
	fd := root
	for _, part := range append([]string{"runtime"}, parts...) {
		if part == "" || part == "." || part == ".." || strings.Contains(part, "/") {
			if fd != root {
				syscall.Close(fd)
			}
			return -1, errors.New("unsafe runtime path")
		}
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if errors.Is(e, syscall.ENOENT) && create {
			e = syscall.Mkdirat(fd, part, 0700)
			if e == nil {
				e = syscall.Fsync(fd)
				next, e = syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
			}
		}
		if fd != root {
			syscall.Close(fd)
		}
		if e != nil {
			return -1, e
		}
		var st syscall.Stat_t
		if syscall.Fstat(next, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || st.Mode&0077 != 0 || int(st.Uid) != r.expectedUID {
			syscall.Close(next)
			return -1, errors.New("unsafe runtime directory")
		}
		fd = next
	}
	return fd, nil
}

func networkName(s State) string {
	return "s2h-net-" + token(s.Resource.Environment, s.Resource.ServerKey, s.Ownership.Value)
}
func networkLabelFor(resource hostcontract.ResourceIdentity, ownership hostcontract.OwnershipIdentity) string {
	return "s2hnet1:" + token(resource.Environment, resource.ServerKey, ownership.Value)
}
func (r *Runtime) admitNetwork(ctx context.Context, s State) error {
	out, err := r.runner.Run(ctx, networkListArgs(s), nil)
	if err != nil {
		return recovery()
	}
	rows := strings.FieldsFunc(string(out), func(c rune) bool { return c == '\n' || c == '\r' })
	if len(rows) == 0 {
		return nil
	}
	if len(rows) != 1 {
		return recovery()
	}
	parts := strings.Split(rows[0], "\t")
	if len(parts) != 3 || parts[0] != networkName(s) {
		return recovery()
	}
	if parts[1] != ownershipLabelFor(s.Resource, s.Ownership, "network", "", "") || parts[2] != networkLabelFor(s.Resource, s.Ownership) {
		return conflict()
	}
	return nil
}
func (r *Runtime) ensureNetwork(ctx context.Context, s State) error {
	if err := r.admitNetwork(ctx, s); err != nil {
		return err
	}
	out, err := r.runner.Run(ctx, networkListArgs(s), nil)
	if err != nil {
		return recovery()
	}
	if strings.TrimSpace(string(out)) != "" {
		return nil
	}
	if err := r.docker(ctx, "network", "create", "--label", "sub2api.host="+ownershipLabelFor(s.Resource, s.Ownership, "network", "", ""), "--label", "sub2api.host.network="+networkLabelFor(s.Resource, s.Ownership), networkName(s)); err != nil {
		if observed := r.admitNetwork(ctx, s); observed == nil {
			out, observed = r.runner.Run(ctx, networkListArgs(s), nil)
			if observed == nil && strings.TrimSpace(string(out)) != "" {
				return nil
			}
		}
		return recovery()
	}
	return nil
}
func networkListArgs(s State) []string {
	return []string{"network", "ls", "--filter", "name=^" + networkName(s) + "$", "--format", "{{.Name}}\t{{index .Labels \"sub2api.host\"}}\t{{index .Labels \"sub2api.host.network\"}}"}
}
func validHostname(v string) bool {
	if len(v) == 0 || len(v) > 253 || !utf8.ValidString(v) || strings.HasSuffix(v, ".") {
		return false
	}
	for _, label := range strings.Split(v, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c == '-' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				return false
			}
		}
	}
	return true
}
func unsafeInline(v string) bool { return v == "" || strings.ContainsAny(v, "\x00\r\n") }
func yamlQuote(v string) string  { return strconv.Quote(v) }
func redisConfigValue(v string) (string, error) {
	if !validLocalPassword(v) {
		return "", errors.New("unsafe redis value")
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f {
			return "", errors.New("unsafe redis value")
		}
	}
	return strconv.Quote(v), nil
}
func validLocalPassword(v string) bool {
	if v == "" || !utf8.ValidString(v) {
		return false
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
func exactDataObservations(got []hostcontract.DataObservation, targets []hostcontract.LocalDataServiceTarget, s State) bool {
	if len(got) != len(targets) {
		return false
	}
	expected := map[hostcontract.DataIdentity]bool{}
	for _, target := range targets {
		expected[localObject(s, target, "").DataIdentity] = true
	}
	for _, observed := range got {
		if !observed.Ready || !expected[observed.Identity] {
			return false
		}
		delete(expected, observed.Identity)
	}
	return len(expected) == 0
}
