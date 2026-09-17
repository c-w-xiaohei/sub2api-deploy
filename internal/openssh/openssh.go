package openssh

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/sshcheck"
)

const maxStderr = 256
const probeTimeout = 10 * time.Second
const hostExecutable = "/nix/var/nix/profiles/sub2api-host/bin/sub2api-host"
const probeRecordVersion = "s2p2"
const maxProbeRecordSize = 2048
const maxReleaseIdentitySize = 256
const machineIdentityDomain = "sub2api-host-machine-identity-v1"

var probeCommand = "sudo -n -- " + hostExecutable + " probe"
var hostCommand = "sudo -n -- " + hostExecutable + " stdio"

var (
	ErrTransport = errors.New("openssh transport")
	ErrHostKey   = errors.New("openssh host key")
	ErrProtocol  = errors.New("openssh protocol")
	ErrRemote    = errors.New("openssh remote")
)

type ProcessError struct {
	Cause    error
	ExitCode int
	HostKey  bool
}

func (e *ProcessError) Error() string { return "ssh process failed" }
func (e *ProcessError) Unwrap() error { return e.Cause }

type RemoteError struct {
	Category hostprotocol.ErrorCategory
	Code     hostprotocol.ErrorCode
}

func (e *RemoteError) Error() string { return string(e.Category) + "/" + string(e.Code) }

type ProbeInfo struct {
	OS              string
	Arch            string
	Machine         string
	InstalledDigest string
	Release         string
}

type Command uint8

const (
	Probe Command = iota + 1
	Host
)

type Transport struct{ start processStart }
type processStart func(context.Context, string, []string, []byte) processResult
type processResult struct {
	stdout, stderr   []byte
	err              error
	exitCode         int
	hostKey          bool
	clientDiagnostic string
}

func New() Transport { return Transport{start: systemStart} }

func (t Transport) Run(ctx context.Context, alias string, command Command, stdin []byte) (hostprotocol.Response, error) {
	if err := sshcheck.ValidateAlias(alias); err != nil {
		return hostprotocol.Response{}, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	remote, ok := remoteCommand(command)
	if !ok {
		return hostprotocol.Response{}, fmt.Errorf("%w: invalid fixed command", ErrTransport)
	}
	if t.start == nil {
		t.start = systemStart
	}
	r := t.start(ctx, "ssh", sshArgs(alias, command, remote), stdin)
	if r.err != nil {
		return hostprotocol.Response{}, processFailure(r)
	}
	v, err := hostprotocol.DecodeResponse(r.stdout)
	if err != nil {
		return hostprotocol.Response{}, ErrProtocol
	}
	if v.Error != nil {
		return v, fmt.Errorf("%w: %w", ErrRemote, &RemoteError{v.Error.Category, v.Error.Code})
	}
	return v, nil
}

func (t Transport) Probe(ctx context.Context, alias string) (ProbeInfo, error) {
	if err := sshcheck.ValidateAlias(alias); err != nil {
		return ProbeInfo{}, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if t.start == nil {
		t.start = systemStart
	}
	r := t.start(ctx, "ssh", sshArgs(alias, Probe, probeCommand), nil)
	if r.err != nil {
		return ProbeInfo{}, processFailure(r)
	}
	return ParseProbeRecord(r.stdout)
}

func ParseProbeRecord(record []byte) (ProbeInfo, error) {
	if len(record) == 0 || len(record) > maxProbeRecordSize || !utf8.Valid(record) {
		return ProbeInfo{}, ErrProtocol
	}
	parts := strings.Split(string(record), "\n")
	if len(parts) != 6 || parts[5] != "" || parts[0] != probeRecordVersion+":Linux" {
		return ProbeInfo{}, ErrProtocol
	}
	if parts[1] != "amd64" && parts[1] != "arm64" || !strings.HasPrefix(parts[2], "mid1:") || !hex64(parts[2][5:]) || !hex64(parts[3]) || !validReleaseIdentity(parts[4]) {
		return ProbeInfo{}, ErrProtocol
	}
	if len(parts[2]) != 69 {
		return ProbeInfo{}, ErrProtocol
	}
	return ProbeInfo{OS: "Linux", Arch: parts[1], Machine: parts[2], InstalledDigest: parts[3], Release: parts[4]}, nil
}

func LocalProbeRecord(machinePath string) ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("unsupported host operating system")
	}
	if machinePath == "" {
		machinePath = "/etc/machine-id"
	}
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return nil, errors.New("unsupported host architecture")
	}
	executable, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return nil, errors.New("running executable unavailable")
	}
	return localProbeRecord(machinePath, "/proc/self/exe", executable, arch)
}

func localProbeRecord(machinePath, executablePath, executableLocation, arch string) ([]byte, error) {
	machineBytes, err := os.ReadFile(machinePath)
	if err != nil {
		return nil, errors.New("machine identity unavailable")
	}
	if len(machineBytes) > 0 && machineBytes[len(machineBytes)-1] == '\n' {
		machineBytes = machineBytes[:len(machineBytes)-1]
	}
	machine := string(machineBytes)
	if len(machine) != 32 || machine == strings.Repeat("0", 32) || !lowerHex(machine) {
		return nil, errors.New("machine identity invalid")
	}
	mac := hmac.New(sha256.New, []byte(machineIdentityDomain))
	_, _ = mac.Write(machineBytes)
	identity := "mid1:" + hex.EncodeToString(mac.Sum(nil))
	if arch != "amd64" && arch != "arm64" {
		return nil, errors.New("unsupported host architecture")
	}
	binary, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, errors.New("running executable unavailable")
	}
	digest := sha256.Sum256(binary)
	releaseBytes, err := os.ReadFile(filepath.Join(filepath.Dir(executableLocation), "..", "share", "sub2api-host", "release"))
	if err != nil || len(releaseBytes) == 0 || len(releaseBytes) > maxReleaseIdentitySize {
		return nil, errors.New("release metadata unavailable")
	}
	if releaseBytes[len(releaseBytes)-1] == '\n' {
		releaseBytes = releaseBytes[:len(releaseBytes)-1]
	}
	release := string(releaseBytes)
	if !validReleaseIdentity(release) {
		return nil, errors.New("release metadata invalid")
	}
	return []byte(fmt.Sprintf("%s:Linux\n%s\n%s\n%x\n%s\n", probeRecordVersion, arch, identity, digest, release)), nil
}

func hex64(v string) bool {
	if len(v) != sha256.Size*2 || !lowerHex(v) {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

func lowerHex(v string) bool {
	for _, r := range v {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func validReleaseIdentity(value string) bool {
	if value == "" || len(value) > maxReleaseIdentitySize || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func sshArgs(alias string, _ Command, remote string) []string {
	return append([]string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "--", alias}, remote)
}

func remoteCommand(c Command) (string, bool) {
	switch c {
	case Probe:
		return probeCommand, true
	case Host:
		return hostCommand, true
	default:
		return "", false
	}
}

func processFailure(r processResult) error {
	if errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded) {
		return r.err
	}
	if r.hostKey {
		return fmt.Errorf("%w: %w", ErrHostKey, &ProcessError{Cause: r.err, ExitCode: r.exitCode, HostKey: true})
	}
	return fmt.Errorf("%w: %w", ErrTransport, &ProcessError{Cause: r.err, ExitCode: r.exitCode})
}
