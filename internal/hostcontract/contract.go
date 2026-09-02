package hostcontract

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const revisionVersion = "tr1"

type ResourceIdentity struct {
	Environment string `json:"environment"`
	ServerKey   string `json:"serverKey"`
}
type ServerTarget struct {
	SSHAlias string `json:"sshAlias"`
}
type MachineIdentity struct {
	Value string `json:"value"`
}
type OwnershipIdentity struct {
	Value string `json:"value"`
}
type Action string

const (
	ActionInspect            Action = "inspect"
	ActionReconcile          Action = "reconcile"
	ActionRetirePreserveData Action = "retire-preserve-data"
)

type DataIdentity struct {
	Kind          string `json:"kind"`
	ProviderID    string `json:"providerId,omitempty"`
	Endpoint      string `json:"endpoint,omitempty"`
	Port          int    `json:"port,omitempty"`
	Database      string `json:"database,omitempty"`
	TLSServerName string `json:"tlsServerName,omitempty"`
}
type DataLink struct {
	Name     string       `json:"name"`
	Identity DataIdentity `json:"identity"`
}
type AppTarget struct {
	ID               string            `json:"id"`
	Image            string            `json:"image"`
	Hostname         string            `json:"hostname"`
	ReadinessPath    string            `json:"readinessPath"`
	DrainTimeout     string            `json:"drainTimeout,omitempty"`
	InitialBootstrap bool              `json:"initialBootstrap,omitempty"`
	RuntimeSettings  map[string]string `json:"runtimeSettings,omitempty"`
	DataLinks        []DataLink        `json:"dataLinks,omitempty"`
}
type LocalDataServiceTarget struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Port        int    `json:"port"`
	Persistence bool   `json:"persistence,omitempty"`
}
type ReverseProxyTarget struct {
	Image     string `json:"image"`
	ACMEEmail string `json:"acmeEmail"`
}
type MicroSocksClientTarget struct {
	ID string `json:"id"`
}
type MicroSocksTarget struct {
	Server  bool                     `json:"server,omitempty"`
	Clients []MicroSocksClientTarget `json:"clients,omitempty"`
}
type TunnelConnectorTarget struct {
	ID       string   `json:"id"`
	TunnelID string   `json:"tunnelId"`
	AppIDs   []string `json:"appIds,omitempty"`
}
type Target struct {
	ReleaseArtifact string                   `json:"releaseArtifact"`
	Apps            []AppTarget              `json:"apps,omitempty"`
	DataServices    []LocalDataServiceTarget `json:"dataServices,omitempty"`
	ReverseProxy    *ReverseProxyTarget      `json:"reverseProxy,omitempty"`
	MicroSocks      *MicroSocksTarget        `json:"microSocks,omitempty"`
	Connectors      []TunnelConnectorTarget  `json:"connectors,omitempty"`
}

type DataCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
type AppSecrets struct {
	InitialAdminPassword string            `json:"initialAdminPassword,omitempty"`
	JWTSecret            string            `json:"jwtSecret,omitempty"`
	TOTPEncryptionKey    string            `json:"totpEncryptionKey,omitempty"`
	AdminAPIKey          string            `json:"adminApiKey,omitempty"`
	RuntimeEnvironment   map[string]string `json:"runtimeEnvironment,omitempty"`
	Postgres             *DataCredentials  `json:"postgres,omitempty"`
	Redis                *DataCredentials  `json:"redis,omitempty"`
}
type LocalDataServiceSecrets struct {
	AdminPassword string `json:"adminPassword"`
}
type ReverseProxySecrets struct {
	DNSChallengeToken string `json:"dnsChallengeToken"`
}
type MicroSocksSecrets struct {
	ServerUsername    string                     `json:"serverUsername,omitempty"`
	ServerPassword    string                     `json:"serverPassword,omitempty"`
	ClientCredentials map[string]DataCredentials `json:"clientCredentials,omitempty"`
}
type TunnelConnectorSecrets struct {
	Token string `json:"token"`
}
type Secrets struct {
	Apps              map[string]AppSecrets              `json:"apps,omitempty"`
	LocalDataServices map[string]LocalDataServiceSecrets `json:"localDataServices,omitempty"`
	ReverseProxy      *ReverseProxySecrets               `json:"reverseProxy,omitempty"`
	MicroSocks        *MicroSocksSecrets                 `json:"microSocks,omitempty"`
	Connectors        map[string]TunnelConnectorSecrets  `json:"connectors,omitempty"`
}

type AppObservation struct {
	ID          string `json:"id"`
	ActiveImage string `json:"activeImage"`
	Ready       bool   `json:"ready"`
}
type DataObservation struct {
	Identity DataIdentity `json:"identity"`
	Ready    bool         `json:"ready"`
}
type StableObservation struct {
	Machine         MachineIdentity   `json:"machine"`
	Ownership       OwnershipIdentity `json:"ownership"`
	HostRelease     string            `json:"hostRelease"`
	AppliedRevision string            `json:"appliedRevision"`
	Drifted         bool              `json:"drifted,omitempty"`
	Ready           bool              `json:"ready"`
	Apps            []AppObservation  `json:"apps,omitempty"`
	Data            []DataObservation `json:"data,omitempty"`
}

type OperationKey struct {
	Resource             ResourceIdentity `json:"resource"`
	Action               Action           `json:"action"`
	TargetRevision       string           `json:"targetRevision"`
	PriorAppliedRevision string           `json:"priorAppliedRevision,omitempty"`
	PriorObservation     string           `json:"priorObservation,omitempty"`
}

func (k OperationKey) Validate() error {
	if !validResource(k.Resource) || k.TargetRevision == "" {
		return fmt.Errorf("operation resource and target revision are required")
	}
	if k.Action != ActionReconcile && k.Action != ActionRetirePreserveData {
		return fmt.Errorf("operation action must be a write action")
	}
	if (k.PriorAppliedRevision == "") == (k.PriorObservation == "") {
		return fmt.Errorf("operation requires exactly one prior precondition")
	}
	return nil
}

type ApprovalKind string

const (
	ApprovalDataLink ApprovalKind = "data-link"
	ApprovalRetire   ApprovalKind = "retire"
)

type ApprovalSubject struct {
	Kind           ApprovalKind      `json:"kind"`
	Environment    string            `json:"environment"`
	Resource       ResourceIdentity  `json:"resource"`
	AppID          string            `json:"appId,omitempty"`
	DataKind       string            `json:"dataKind,omitempty"`
	OldData        DataIdentity      `json:"oldData,omitempty"`
	NewData        DataIdentity      `json:"newData,omitempty"`
	Machine        MachineIdentity   `json:"machine,omitempty"`
	Ownership      OwnershipIdentity `json:"ownership,omitempty"`
	TargetRevision string            `json:"targetRevision"`
	PreserveData   bool              `json:"preserveData,omitempty"`
}

func (a ApprovalSubject) Validate() error {
	if a.Environment == "" || a.Environment != a.Resource.Environment || !validResource(a.Resource) || a.TargetRevision == "" {
		return fmt.Errorf("approval scope is invalid")
	}
	switch a.Kind {
	case ApprovalDataLink:
		if a.AppID == "" || (a.DataKind != "postgres" && a.DataKind != "redis") || validateData(a.OldData) != nil || validateData(a.NewData) != nil || a.OldData.Kind != a.DataKind || a.NewData.Kind != a.DataKind || a.OldData == a.NewData || a.Machine.Value != "" || a.Ownership.Value != "" || a.PreserveData {
			return fmt.Errorf("data-link approval is invalid")
		}
	case ApprovalRetire:
		if a.AppID != "" || a.DataKind != "" || a.Machine.Value == "" || a.Ownership.Value == "" || !a.PreserveData {
			return fmt.Errorf("retire approval is invalid")
		}
	default:
		return fmt.Errorf("approval kind is invalid")
	}
	return nil
}
func (a ApprovalSubject) MatchesReconcileTarget(k OperationKey, target Target) bool {
	if !a.Matches(k, a.AppID) || a.Kind != ApprovalDataLink {
		return false
	}
	for _, app := range target.Apps {
		if app.ID == a.AppID {
			for _, link := range app.DataLinks {
				if link.Identity.Kind == a.DataKind && link.Identity == a.NewData {
					return true
				}
			}
		}
	}
	return false
}
func (a ApprovalSubject) Matches(k OperationKey, appID string) bool {
	if a.Validate() != nil || k.Validate() != nil || a.Resource != k.Resource || a.TargetRevision != k.TargetRevision {
		return false
	}
	return (a.Kind == ApprovalDataLink && k.Action == ActionReconcile && a.AppID == appID) || (a.Kind == ApprovalRetire && k.Action == ActionRetirePreserveData)
}

type RevisionKey []byte

func (key RevisionKey) Validate() error {
	if len(key) != sha256.Size {
		return fmt.Errorf("revision key is invalid")
	}
	return nil
}

type Revision struct {
	KeyID      string
	Commitment string
}

func (r Revision) String() string { return revisionVersion + ":" + r.KeyID + ":" + r.Commitment }
func ParseRevision(value string) (Revision, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] != revisionVersion || len(parts[1]) != 16 || len(parts[2]) != sha256.Size*2 {
		return Revision{}, fmt.Errorf("invalid target revision")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return Revision{}, fmt.Errorf("invalid target revision")
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return Revision{}, fmt.Errorf("invalid target revision")
	}
	return Revision{KeyID: parts[1], Commitment: parts[2]}, nil
}

func TargetRevision(key RevisionKey, resource ResourceIdentity, target Target, secrets Secrets) (string, error) {
	if key.Validate() != nil || !validResource(resource) {
		return "", fmt.Errorf("invalid revision input")
	}
	target, secrets = normalize(target, secrets)
	if err := validate(target, secrets); err != nil {
		return "", fmt.Errorf("invalid revision input")
	}
	payload, err := canonicalJSON(struct {
		Domain   string           `json:"domain"`
		Resource ResourceIdentity `json:"resource"`
		Target   Target           `json:"target"`
		Secrets  Secrets          `json:"secrets"`
	}{"sub2api-host-target-revision-v1", resource, target, secrets})
	if err != nil {
		return "", fmt.Errorf("invalid revision input")
	}
	idMAC := hmac.New(sha256.New, key)
	_, _ = idMAC.Write([]byte("sub2api-host-revision-key-id-v1"))
	keyID := hex.EncodeToString(idMAC.Sum(nil)[:8])
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return Revision{KeyID: keyID, Commitment: hex.EncodeToString(mac.Sum(nil))}.String(), nil
}

func normalize(target Target, secrets Secrets) (Target, Secrets) {
	target.Apps = append([]AppTarget(nil), target.Apps...)
	sort.Slice(target.Apps, func(i, j int) bool { return target.Apps[i].ID < target.Apps[j].ID })
	for i := range target.Apps {
		target.Apps[i].RuntimeSettings = copyStrings(target.Apps[i].RuntimeSettings)
		target.Apps[i].DataLinks = append([]DataLink(nil), target.Apps[i].DataLinks...)
		sort.Slice(target.Apps[i].DataLinks, func(a, b int) bool { return target.Apps[i].DataLinks[a].Name < target.Apps[i].DataLinks[b].Name })
	}
	if target.MicroSocks != nil {
		microSocks := *target.MicroSocks
		target.MicroSocks = &microSocks
		target.MicroSocks.Clients = append([]MicroSocksClientTarget(nil), microSocks.Clients...)
		sort.Slice(target.MicroSocks.Clients, func(i, j int) bool { return target.MicroSocks.Clients[i].ID < target.MicroSocks.Clients[j].ID })
		if len(target.MicroSocks.Clients) == 0 {
			target.MicroSocks.Clients = nil
		}
	}
	target.Connectors = append([]TunnelConnectorTarget(nil), target.Connectors...)
	for i := range target.Connectors {
		target.Connectors[i].AppIDs = append([]string(nil), target.Connectors[i].AppIDs...)
		sort.Strings(target.Connectors[i].AppIDs)
		if len(target.Connectors[i].AppIDs) == 0 {
			target.Connectors[i].AppIDs = nil
		}
	}
	target.DataServices = append([]LocalDataServiceTarget(nil), target.DataServices...)
	sort.Slice(target.DataServices, func(i, j int) bool { return target.DataServices[i].ID < target.DataServices[j].ID })
	sort.Slice(target.Connectors, func(i, j int) bool { return target.Connectors[i].ID < target.Connectors[j].ID })
	if len(target.Apps) == 0 {
		target.Apps = nil
	}
	if len(target.DataServices) == 0 {
		target.DataServices = nil
	}
	if len(target.Connectors) == 0 {
		target.Connectors = nil
	}
	if len(secrets.Apps) == 0 {
		secrets.Apps = nil
	}
	if len(secrets.LocalDataServices) == 0 {
		secrets.LocalDataServices = nil
	}
	if len(secrets.Connectors) == 0 {
		secrets.Connectors = nil
	}
	secrets.Apps = copyAppSecrets(secrets.Apps)
	secrets.LocalDataServices = copyLocalDataSecrets(secrets.LocalDataServices)
	secrets.Connectors = copyConnectorSecrets(secrets.Connectors)
	if secrets.MicroSocks != nil {
		microSocks := *secrets.MicroSocks
		microSocks.ClientCredentials = copyCredentials(microSocks.ClientCredentials)
		secrets.MicroSocks = &microSocks
		if microSocks.ServerUsername == "" && microSocks.ServerPassword == "" && len(microSocks.ClientCredentials) == 0 {
			secrets.MicroSocks = nil
		}
	}
	if target.MicroSocks != nil && !target.MicroSocks.Server && len(target.MicroSocks.Clients) == 0 {
		target.MicroSocks = nil
	}
	return target, secrets
}

// NormalizeTargetSecrets returns the canonical target/secrets representation used
// by TargetRevision. Callers that persist or authenticate Host inputs must use it.
func NormalizeTargetSecrets(target Target, secrets Secrets) (Target, Secrets) { return normalize(target, secrets) }
func copyStrings(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	copy := make(map[string]string, len(values))
	for k, v := range values {
		copy[k] = v
	}
	return copy
}
func copyCredentials(values map[string]DataCredentials) map[string]DataCredentials {
	if len(values) == 0 {
		return nil
	}
	copy := make(map[string]DataCredentials, len(values))
	for k, v := range values {
		copy[k] = v
	}
	return copy
}
func copyAppSecrets(values map[string]AppSecrets) map[string]AppSecrets {
	if len(values) == 0 {
		return nil
	}
	copy := make(map[string]AppSecrets, len(values))
	for k, v := range values {
		v.RuntimeEnvironment = copyStrings(v.RuntimeEnvironment)
		copy[k] = v
	}
	return copy
}
func copyLocalDataSecrets(values map[string]LocalDataServiceSecrets) map[string]LocalDataServiceSecrets {
	if len(values) == 0 {
		return nil
	}
	copy := make(map[string]LocalDataServiceSecrets, len(values))
	for k, v := range values {
		copy[k] = v
	}
	return copy
}
func copyConnectorSecrets(values map[string]TunnelConnectorSecrets) map[string]TunnelConnectorSecrets {
	if len(values) == 0 {
		return nil
	}
	copy := make(map[string]TunnelConnectorSecrets, len(values))
	for k, v := range values {
		copy[k] = v
	}
	return copy
}
func validate(target Target, secrets Secrets) error {
	if target.ReleaseArtifact == "" {
		return fmt.Errorf("release")
	}
	apps := map[string]bool{}
	services := map[string]bool{}
	connectors := map[string]bool{}
	for _, a := range target.Apps {
		if a.ID == "" || a.Image == "" || a.Hostname == "" || a.ReadinessPath == "" || apps[a.ID] {
			return fmt.Errorf("app")
		}
		apps[a.ID] = true
		links := map[string]bool{}
		for _, l := range a.DataLinks {
			if l.Name == "" || links[l.Name] || validateData(l.Identity) != nil {
				return fmt.Errorf("data link")
			}
			links[l.Name] = true
		}
	}
	for _, s := range target.DataServices {
		if s.ID == "" || services[s.ID] || (s.Type != "postgres" && s.Type != "redis") || s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("service")
		}
		services[s.ID] = true
	}
	for _, c := range target.Connectors {
		if c.ID == "" || c.TunnelID == "" || connectors[c.ID] {
			return fmt.Errorf("connector")
		}
		connectors[c.ID] = true
		seen := map[string]bool{}
		for _, id := range c.AppIDs {
			if id == "" || seen[id] || !apps[id] {
				return fmt.Errorf("connector app")
			}
			seen[id] = true
		}
	}
	for id := range secrets.Apps {
		if !apps[id] {
			return fmt.Errorf("app secret")
		}
	}
	for id := range secrets.LocalDataServices {
		if !services[id] {
			return fmt.Errorf("service secret")
		}
	}
	for id := range secrets.Connectors {
		if !connectors[id] {
			return fmt.Errorf("connector secret")
		}
	}
	if (target.ReverseProxy == nil) != (secrets.ReverseProxy == nil) {
		return fmt.Errorf("proxy secret scope")
	}
	if secrets.MicroSocks != nil && target.MicroSocks == nil {
		return fmt.Errorf("microsocks secret scope")
	}
	if target.MicroSocks != nil {
		clients := map[string]bool{}
		for _, c := range target.MicroSocks.Clients {
			if c.ID == "" || clients[c.ID] {
				return fmt.Errorf("microsocks client")
			}
			clients[c.ID] = true
		}
		if secrets.MicroSocks != nil {
			if !target.MicroSocks.Server && (secrets.MicroSocks.ServerUsername != "" || secrets.MicroSocks.ServerPassword != "") {
				return fmt.Errorf("microsocks server secret")
			}
			for id := range secrets.MicroSocks.ClientCredentials {
				if !clients[id] {
					return fmt.Errorf("microsocks client secret")
				}
			}
		}
	}
	if !validTargetStrings(target) || !validSecretStrings(secrets) {
		return fmt.Errorf("utf8")
	}
	return nil
}
func validateData(d DataIdentity) error {
	if (d.Kind != "postgres" && d.Kind != "redis") || d.ProviderID == "" || d.Endpoint == "" || d.Port < 1 || d.Port > 65535 || d.Database == "" {
		return fmt.Errorf("identity")
	}
	if d.Kind == "postgres" && d.TLSServerName == "" {
		return fmt.Errorf("identity")
	}
	return nil
}
func validResource(r ResourceIdentity) bool {
	return r.Environment != "" && r.ServerKey != "" && utf8.ValidString(r.Environment) && utf8.ValidString(r.ServerKey)
}
func ValidateTarget(target Target, secrets Secrets) error {
	target, secrets = normalize(target, secrets)
	return validate(target, secrets)
}
func (o StableObservation) Validate() error {
	if o.Machine.Value == "" || o.Ownership.Value == "" || o.HostRelease == "" || o.AppliedRevision == "" || !utf8.ValidString(o.Machine.Value) || !utf8.ValidString(o.Ownership.Value) || !utf8.ValidString(o.HostRelease) {
		return fmt.Errorf("observation")
	}
	if _, e := ParseRevision(o.AppliedRevision); e != nil {
		return fmt.Errorf("observation")
	}
	for _, a := range o.Apps {
		if a.ID == "" || a.ActiveImage == "" || !utf8.ValidString(a.ID) || !utf8.ValidString(a.ActiveImage) {
			return fmt.Errorf("observation")
		}
	}
	for _, d := range o.Data {
		if validateData(d.Identity) != nil {
			return fmt.Errorf("observation")
		}
	}
	return nil
}
func validTargetStrings(v Target) bool {
	valid := utf8.ValidString
	if !valid(v.ReleaseArtifact) {
		return false
	}
	for _, a := range v.Apps {
		if !valid(a.ID) || !valid(a.Image) || !valid(a.Hostname) || !valid(a.ReadinessPath) || !valid(a.DrainTimeout) {
			return false
		}
		for k, x := range a.RuntimeSettings {
			if !valid(k) || !valid(x) {
				return false
			}
		}
		for _, l := range a.DataLinks {
			if !valid(l.Name) || !validDataStrings(l.Identity) {
				return false
			}
		}
	}
	for _, s := range v.DataServices {
		if !valid(s.ID) || !valid(s.Type) {
			return false
		}
	}
	if v.ReverseProxy != nil && (!valid(v.ReverseProxy.Image) || !valid(v.ReverseProxy.ACMEEmail)) {
		return false
	}
	if v.MicroSocks != nil {
		for _, c := range v.MicroSocks.Clients {
			if !valid(c.ID) {
				return false
			}
		}
	}
	for _, c := range v.Connectors {
		if !valid(c.ID) || !valid(c.TunnelID) {
			return false
		}
	}
	return true
}
func validSecretStrings(v Secrets) bool {
	valid := utf8.ValidString
	for id, a := range v.Apps {
		if !valid(id) || !valid(a.InitialAdminPassword) || !valid(a.JWTSecret) || !valid(a.TOTPEncryptionKey) || !valid(a.AdminAPIKey) {
			return false
		}
		for k, x := range a.RuntimeEnvironment {
			if !valid(k) || !valid(x) {
				return false
			}
		}
		if a.Postgres != nil && (!valid(a.Postgres.Username) || !valid(a.Postgres.Password)) {
			return false
		}
		if a.Redis != nil && (!valid(a.Redis.Username) || !valid(a.Redis.Password)) {
			return false
		}
	}
	for id, s := range v.LocalDataServices {
		if !valid(id) || !valid(s.AdminPassword) {
			return false
		}
	}
	if v.ReverseProxy != nil && !valid(v.ReverseProxy.DNSChallengeToken) {
		return false
	}
	if v.MicroSocks != nil {
		if !valid(v.MicroSocks.ServerUsername) || !valid(v.MicroSocks.ServerPassword) {
			return false
		}
		for id, c := range v.MicroSocks.ClientCredentials {
			if !valid(id) || !valid(c.Username) || !valid(c.Password) {
				return false
			}
		}
	}
	for id, c := range v.Connectors {
		if !valid(id) || !valid(c.Token) {
			return false
		}
	}
	return true
}
func validDataStrings(v DataIdentity) bool {
	return utf8.ValidString(v.Kind) && utf8.ValidString(v.ProviderID) && utf8.ValidString(v.Endpoint) && utf8.ValidString(v.Database) && utf8.ValidString(v.TLSServerName)
}
func canonicalJSON(v any) ([]byte, error)       { return json.Marshal(v) }
func decodeCanonicalJSON(b []byte, v any) error { return json.Unmarshal(b, v) }
