package hostruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

// nftRunner intentionally has no executable or argv input. The only commands
// it can issue are the fixed inspection, syntax check, and atomic apply forms.
type nftRunner interface {
	Run(context.Context, []string, []byte) ([]byte, error)
}

type execNFTRunner struct{}

type nftState uint8

const (
	nftExact nftState = iota
	nftOld
	nftForeign
	nftMalformed
)

type nftPolicy struct {
	Groups []nftSocketGroup
}
type nftSocketGroup struct {
	Family, Destination string
	Port                int
	Sources             []string
}

var errNftNotFound = errors.New("owned nft table not found")

func (execNFTRunner) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	if !validNftArgs(argv) {
		return nil, errors.New("invalid nft command")
	}
	cmd := exec.CommandContext(ctx, "nft", argv...)
	cmd.Stdin = bytes.NewReader(stdin)
	var output limitedBuffer
	output.limit = make(chan struct{})
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if err == nil && !output.full.Load() {
		return output.Bytes(), nil
	}
	// nft reports a missing named table with this exact bounded diagnostic. All
	// other failures, including permission and parse failures, remain opaque.
	if len(argv) == 5 && strings.Contains(strings.ToLower(string(output.Bytes())), "no such file or directory") {
		return nil, errNftNotFound
	}
	return nil, errors.New("nft command failed")
}

func validNftArgs(argv []string) bool {
	return len(argv) == 5 && argv[0] == "-j" && argv[1] == "list" && argv[2] == "table" && argv[3] == "inet" || len(argv) == 3 && argv[0] == "-c" && argv[1] == "-f" && argv[2] == "-" || len(argv) == 2 && argv[0] == "-f" && argv[1] == "-"
}

func (r *Runtime) reconcileNft(ctx context.Context, s State, targets []hostcontract.LocalDataServiceTarget) error {
	policy, err := nftPolicyForTargets(targets)
	if err != nil {
		return operationFailed()
	}
	desired, err := nftRender(s, policy)
	if err != nil {
		return operationFailed()
	}
	if len(desired) == 0 {
		return r.removeNft(ctx, s, true)
	}
	current, err := r.nft.Run(ctx, []string{"-j", "list", "table", "inet", nftTableName(s)}, nil)
	state := nftMalformed
	if err == nil {
		state = classifyNftJSON(current, s, policy)
	}
	if state == nftForeign {
		return conflict()
	}
	if err != nil && !errors.Is(err, errNftNotFound) {
		return recovery()
	}
	if err == nil && state == nftMalformed {
		return recovery()
	}
	if state == nftExact {
		return nil
	}
	apply := desired
	if state == nftOld {
		apply = append([]byte("delete table inet "+nftTableName(s)+"\n"), desired...)
	}
	if _, err = r.nft.Run(ctx, []string{"-c", "-f", "-"}, apply); err != nil {
		return operationFailed()
	}
	if _, err = r.nft.Run(ctx, []string{"-f", "-"}, apply); err != nil {
		observed, observeErr := r.nft.Run(ctx, []string{"-j", "list", "table", "inet", nftTableName(s)}, nil)
		observedState := nftMalformed
		if observeErr == nil {
			observedState = classifyNftJSON(observed, s, policy)
		}
		if observeErr != nil || observedState != nftExact {
			if observedState == nftOld {
				return operationFailed()
			}
			if observedState == nftForeign {
				return conflict()
			}
			return recovery()
		}
		return nil
	}
	observed, err := r.nft.Run(ctx, []string{"-j", "list", "table", "inet", nftTableName(s)}, nil)
	if err != nil || classifyNftJSON(observed, s, policy) != nftExact {
		return recovery()
	}
	return nil
}

func (r *Runtime) removeNft(ctx context.Context, s State, allowAbsent bool) error {
	observed, err := r.nft.Run(ctx, []string{"-j", "list", "table", "inet", nftTableName(s)}, nil)
	if err != nil {
		if allowAbsent && errors.Is(err, errNftNotFound) {
			return nil
		}
		return recovery()
	}
	state := classifyNftJSON(observed, s, nftPolicy{})
	if state == nftForeign {
		return conflict()
	}
	if state == nftMalformed {
		return recovery()
	}
	apply := []byte("delete table inet " + nftTableName(s) + "\n")
	if _, err = r.nft.Run(ctx, []string{"-c", "-f", "-"}, apply); err != nil {
		return operationFailed()
	}
	if _, err = r.nft.Run(ctx, []string{"-f", "-"}, apply); err != nil {
		after, inspectErr := r.nft.Run(ctx, []string{"-j", "list", "table", "inet", nftTableName(s)}, nil)
		if errors.Is(inspectErr, errNftNotFound) {
			return nil
		}
		if inspectErr == nil {
			state := classifyNftJSON(after, s, nftPolicy{})
			if state == nftOld {
				return operationFailed()
			}
			if state == nftForeign {
				return conflict()
			}
		}
		return recovery()
	}
	if _, err = r.nft.Run(ctx, []string{"-j", "list", "table", "inet", nftTableName(s)}, nil); !errors.Is(err, errNftNotFound) {
		return recovery()
	}
	return nil
}

func (r *Runtime) observeNft(ctx context.Context, s State, inv inventory) error {
	targets := make([]hostcontract.LocalDataServiceTarget, 0)
	for _, object := range inv.Objects {
		if object.Role == "local-data" && len(object.Bindings) != 0 {
			targets = append(targets, hostcontract.LocalDataServiceTarget{ID: object.AppToken, Type: object.Type, Port: object.Port, Bindings: object.Bindings})
		}
	}
	policy, err := nftPolicyForTargets(targets)
	if err != nil {
		return err
	}
	observed, err := r.nft.Run(ctx, []string{"-j", "list", "table", "inet", nftTableName(s)}, nil)
	if len(policy.Groups) == 0 {
		if errors.Is(err, errNftNotFound) {
			return nil
		}
		return errors.New("unexpected nft state")
	}
	if err != nil || classifyNftJSON(observed, s, policy) != nftExact {
		return errors.New("nft drift")
	}
	return nil
}

func classifyNftJSON(data []byte, s State, wanted nftPolicy) nftState {
	if len(data) == 0 || len(data) > maxCommandOutput || duplicateKey(data) {
		return nftMalformed
	}
	var document struct {
		Nftables []json.RawMessage `json:"nftables"`
	}
	if json.Unmarshal(data, &document) != nil || len(document.Nftables) == 0 {
		return nftMalformed
	}
	var table json.RawMessage
	var chain json.RawMessage
	var rules []json.RawMessage
	phase := 0
	for _, item := range document.Nftables {
		var envelope map[string]json.RawMessage
		if json.Unmarshal(item, &envelope) != nil || len(envelope) != 1 {
			return nftMalformed
		}
		if raw, ok := envelope["metainfo"]; ok {
			if phase != 0 || !validJSONObject(raw, "version", "release_name", "json_schema_version") {
				return nftMalformed
			}
			continue
		}
		for kind, raw := range envelope {
			switch kind {
			case "table":
				if phase != 0 || table != nil {
					return nftMalformed
				}
				table = raw
				phase = 1
			case "chain":
				if phase != 1 || chain != nil {
					return nftMalformed
				}
				chain = raw
				phase = 2
			case "rule":
				if phase != 2 {
					return nftMalformed
				}
				rules = append(rules, raw)
			default:
				return nftMalformed
			}
		}
	}
	if table == nil {
		return nftMalformed
	}
	var t struct {
		Family, Name, Comment string
		Handle                json.RawMessage `json:"handle"`
	}
	if !validJSONObject(table, "family", "name", "comment", "handle") || json.Unmarshal(table, &t) != nil || t.Family != "inet" || t.Name != nftTableName(s) || !validHandle(t.Handle) {
		return nftMalformed
	}
	if t.Comment != nftOwnershipComment(s) {
		return nftForeign
	}
	if chain == nil {
		return nftMalformed
	}
	var c struct {
		Family, Table, Name, Type, Hook, Policy, Comment string
		Prio                                             int             `json:"prio"`
		Handle                                           json.RawMessage `json:"handle"`
	}
	if !validJSONObject(chain, "family", "table", "name", "type", "hook", "prio", "policy", "comment", "handle") || json.Unmarshal(chain, &c) != nil || c.Family != "inet" || c.Table != nftTableName(s) || c.Name != "prerouting" || c.Type != "filter" || c.Hook != "prerouting" || c.Prio != -110 || c.Policy != "accept" || !validHandle(c.Handle) {
		return nftMalformed
	}
	observed, ok := nftPolicyFromRules(rules, s)
	if !ok || c.Comment != nftPolicyCommentFor(observed) {
		return nftMalformed
	}
	if reflect.DeepEqual(observed, wanted) {
		return nftExact
	}
	return nftOld
}

func validJSONObject(raw []byte, allowed ...string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return false
	}
	for key := range object {
		known := false
		for _, field := range allowed {
			known = known || key == field
		}
		if !known {
			return false
		}
	}
	return true
}

func validHandle(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var value uint64
	return json.Unmarshal(raw, &value) == nil
}

func nftPolicyFromRules(rules []json.RawMessage, s State) (nftPolicy, bool) {
	var policy nftPolicy
	for i := 0; i < len(rules); {
		var rule struct {
			Family, Table, Chain string
			Expr                 json.RawMessage `json:"expr"`
			Handle               json.RawMessage `json:"handle"`
		}
		if !validJSONObject(rules[i], "family", "table", "chain", "expr", "handle") || json.Unmarshal(rules[i], &rule) != nil || rule.Family != "inet" || rule.Table != nftTableName(s) || rule.Chain != "prerouting" || !validHandle(rule.Handle) {
			return nftPolicy{}, false
		}
		group, source, verdict, ok := nftRuleParts(rule.Expr)
		if !ok {
			return nftPolicy{}, false
		}
		if verdict != "accept" || source == "" {
			return nftPolicy{}, false
		}
		group.Sources = []string{source}
		for i++; i < len(rules); i++ {
			if !validJSONObject(rules[i], "family", "table", "chain", "expr", "handle") || json.Unmarshal(rules[i], &rule) != nil || rule.Family != "inet" || rule.Table != nftTableName(s) || rule.Chain != "prerouting" || !validHandle(rule.Handle) {
				return nftPolicy{}, false
			}
			next, nextSource, nextVerdict, ok := nftRuleParts(rule.Expr)
			if !ok || next.Family != group.Family || next.Destination != group.Destination || next.Port != group.Port {
				return nftPolicy{}, false
			}
			if nextVerdict == "drop" && nextSource == "" {
				break
			}
			if nextVerdict != "accept" || nextSource == "" || nextSource <= group.Sources[len(group.Sources)-1] {
				return nftPolicy{}, false
			}
			group.Sources = append(group.Sources, nextSource)
		}
		if i == len(rules) {
			return nftPolicy{}, false
		}
		policy.Groups = append(policy.Groups, group)
		i++
	}
	for i := 1; i < len(policy.Groups); i++ {
		if policyGroupKey(policy.Groups[i-1]) >= policyGroupKey(policy.Groups[i]) {
			return nftPolicy{}, false
		}
	}
	return policy, true
}

func nftRuleParts(raw json.RawMessage) (nftSocketGroup, string, string, bool) {
	var expressions []json.RawMessage
	if json.Unmarshal(raw, &expressions) != nil || len(expressions) < 3 || len(expressions) > 4 {
		return nftSocketGroup{}, "", "", false
	}
	var group nftSocketGroup
	index := 0
	if len(expressions) == 4 {
		family, field, right, ok := nftMatch(expressions[index])
		if !ok || field != "saddr" {
			return group, "", "", false
		}
		group.Family = family
		ip := net.ParseIP(right)
		if ip == nil || right != ip.String() || (family == "ip") != (ip.To4() != nil) {
			return group, "", "", false
		}
		index++
		group.Sources = []string{right}
	}
	family, field, destination, ok := nftMatch(expressions[index])
	if !ok || field != "daddr" {
		return group, "", "", false
	}
	group.Family = family
	ip := net.ParseIP(destination)
	if ip == nil || destination != ip.String() || (family == "ip") != (ip.To4() != nil) {
		return group, "", "", false
	}
	group.Destination = destination
	_, field, port, ok := nftMatch(expressions[index+1])
	if !ok || field != "dport" {
		return group, "", "", false
	}
	parsed, err := strconv.Atoi(port)
	if err != nil || parsed < 1 || parsed > 65535 {
		return group, "", "", false
	}
	group.Port = parsed
	var verdict map[string]json.RawMessage
	if json.Unmarshal(expressions[index+2], &verdict) != nil || len(verdict) != 1 {
		return group, "", "", false
	}
	var value string
	for candidate, terminal := range verdict {
		if (candidate != "accept" && candidate != "drop") || string(terminal) != "null" {
			return group, "", "", false
		}
		value = candidate
	}
	if value == "" {
		return group, "", "", false
	}
	source := ""
	if len(group.Sources) == 1 {
		source = group.Sources[0]
	}
	if !jsonEqual(raw, nftRule(group, source, value)) {
		return nftSocketGroup{}, "", "", false
	}
	return group, source, value, true
}

func jsonEqual(a, b []byte) bool {
	var av, bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && reflect.DeepEqual(av, bv)
}

func nftMatch(raw json.RawMessage) (string, string, string, bool) {
	var value struct {
		Match struct {
			Op   string `json:"op"`
			Left struct {
				Payload struct{ Protocol, Field string } `json:"payload"`
			} `json:"left"`
			Right json.RawMessage `json:"right"`
		} `json:"match"`
	}
	if !validJSONObject(raw, "match") || json.Unmarshal(raw, &value) != nil || value.Match.Op != "==" || (value.Match.Left.Payload.Protocol != "ip" && value.Match.Left.Payload.Protocol != "ip6" && value.Match.Left.Payload.Protocol != "tcp") {
		return "", "", "", false
	}
	var text string
	if json.Unmarshal(value.Match.Right, &text) == nil {
		return value.Match.Left.Payload.Protocol, value.Match.Left.Payload.Field, text, true
	}
	var number int
	if json.Unmarshal(value.Match.Right, &number) == nil {
		return value.Match.Left.Payload.Protocol, value.Match.Left.Payload.Field, strconv.Itoa(number), true
	}
	return "", "", "", false
}

func nftTableName(s State) string {
	return "s2h_" + token(s.Resource.Environment, s.Resource.ServerKey, s.Ownership.Value)
}
func nftPolicyFor(s State, targets []hostcontract.LocalDataServiceTarget) nftPolicy {
	p, _ := nftPolicyForTargets(targets)
	return p
}
func nftPolicyForTargets(targets []hostcontract.LocalDataServiceTarget) (nftPolicy, error) {
	var p nftPolicy
	for _, target := range targets {
		for _, binding := range target.Bindings {
			ip := net.ParseIP(binding.Address)
			if ip == nil || binding.Address != ip.String() {
				return nftPolicy{}, errors.New("invalid binding")
			}
			family := "ip"
			if ip.To4() == nil {
				family = "ip6"
			}
			if target.Port < 1 || target.Port > 65535 || len(binding.AllowedSources) == 0 {
				return nftPolicy{}, errors.New("invalid binding")
			}
			group := nftSocketGroup{Family: family, Destination: binding.Address, Port: target.Port, Sources: append([]string(nil), binding.AllowedSources...)}
			for _, source := range group.Sources {
				sourceIP := net.ParseIP(source)
				if sourceIP == nil || source != sourceIP.String() || (family == "ip") != (sourceIP.To4() != nil) {
					return nftPolicy{}, errors.New("invalid binding")
				}
			}
			sort.Strings(group.Sources)
			for i := 1; i < len(group.Sources); i++ {
				if group.Sources[i-1] == group.Sources[i] {
					return nftPolicy{}, errors.New("invalid binding")
				}
			}
			p.Groups = append(p.Groups, group)
		}
	}
	sort.Slice(p.Groups, func(i, j int) bool {
		a, b := p.Groups[i], p.Groups[j]
		return policyGroupKey(a) < policyGroupKey(b)
	})
	for i := 1; i < len(p.Groups); i++ {
		if policyGroupKey(p.Groups[i-1]) == policyGroupKey(p.Groups[i]) {
			return nftPolicy{}, errors.New("duplicate socket")
		}
	}
	return p, nil
}
func nftPolicyCommentFor(p nftPolicy) string {
	rules, _ := json.Marshal(nftRuleExpressions(p))
	return "sub2api-host:nft-policy:v1:" + token(string(rules))
}
func nftRuleExpressions(p nftPolicy) []json.RawMessage {
	var values []json.RawMessage
	for _, group := range p.Groups {
		for _, source := range group.Sources {
			values = append(values, nftRule(group, source, "accept"))
		}
		values = append(values, nftRule(group, "", "drop"))
	}
	return values
}
func nftRule(group nftSocketGroup, source, verdict string) json.RawMessage {
	expr := []any{}
	if source != "" {
		expr = append(expr, map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"payload": map[string]any{"protocol": group.Family, "field": "saddr"}}, "right": source}})
	}
	expr = append(expr, map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"payload": map[string]any{"protocol": group.Family, "field": "daddr"}}, "right": group.Destination}}, map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"payload": map[string]any{"protocol": "tcp", "field": "dport"}}, "right": group.Port}}, map[string]any{verdict: nil})
	b, _ := json.Marshal(expr)
	return b
}
func policyGroupKey(group nftSocketGroup) string {
	return group.Family + "\x00" + group.Destination + "\x00" + strconv.Itoa(group.Port)
}
func nftRender(s State, p nftPolicy) ([]byte, error) { return nftTransaction(s, policyTargets(p)) }
func policyTargets(p nftPolicy) []hostcontract.LocalDataServiceTarget {
	var targets []hostcontract.LocalDataServiceTarget
	for i, group := range p.Groups {
		targets = append(targets, hostcontract.LocalDataServiceTarget{ID: strconv.Itoa(i), Port: group.Port, Bindings: []hostcontract.LocalDataBinding{{Address: group.Destination, AllowedSources: group.Sources}}})
	}
	return targets
}
func nftOwnershipComment(s State) string {
	return "sub2api-host:nft:v1:" + token(s.Resource.Environment, s.Resource.ServerKey, s.Ownership.Value)
}

func nftTransaction(s State, targets []hostcontract.LocalDataServiceTarget) ([]byte, error) {
	p, err := nftPolicyForTargets(targets)
	if err != nil {
		return nil, err
	}
	var rules []string
	for _, group := range p.Groups {
		for _, source := range group.Sources {
			rules = append(rules, "  "+group.Family+" saddr "+source+" "+group.Family+" daddr "+group.Destination+" tcp dport "+strconv.Itoa(group.Port)+" accept")
		}
		rules = append(rules, "  "+group.Family+" daddr "+group.Destination+" tcp dport "+strconv.Itoa(group.Port)+" drop")
	}
	if len(rules) == 0 {
		return nil, nil
	}
	policy := nftPolicyCommentFor(p)
	return []byte("table inet " + nftTableName(s) + " { comment \"" + nftOwnershipComment(s) + "\"\n chain prerouting {\n  comment \"" + policy + "\"\n  type filter hook prerouting priority -110; policy accept;\n" + strings.Join(rules, "\n") + "\n }\n}\n"), nil
}
