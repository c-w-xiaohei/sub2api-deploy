package openssh

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/artifact"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/sshcheck"
)

const maxStderr = 256
const probeTimeout = 10 * time.Second
const stagePath = "/usr/local/libexec/.sub2api-host.stage"
const finalPath = "/usr/local/libexec/sub2api-host"
const machineIdentityKey = "sub2api-host-machine-identity-v1"

var probeCommand = probeScript("/etc/machine-id", finalPath)

func probeScript(machinePath, installedPath string) string {
	return fmt.Sprintf(`set -eu
[ "$(uname -s)" = Linux ]
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) exit 64 ;;
esac
[ -r %s ]
bytes=$(wc -c < %s)
case "$bytes" in 32|33) ;; *) exit 64 ;; esac
machine=$(cat %s)
case "$machine" in
  00000000000000000000000000000000|*[!0123456789abcdef]*) exit 64 ;;
esac
[ ${#machine} -eq 32 ]
command -v openssl >/dev/null 2>&1
identity=$(printf %%s "$machine" | openssl dgst -sha256 -mac HMAC -macopt key:%s | awk '{print $NF}')
case "$identity" in *[!0123456789abcdef]*) exit 64 ;; esac
[ ${#identity} -eq 64 ]
digest=missing
if [ -f %s ]; then
  digest=$(sha256sum %s | awk '{print $1}')
  case "$digest" in *[!0123456789abcdef]*) exit 64 ;; esac
  [ ${#digest} -eq 64 ]
fi
printf 's2p1:Linux\n%%s\n%%s\n%%s\n' "$arch" "$identity" "$digest"
`, shellQuote(machinePath), shellQuote(machinePath), shellQuote(machinePath), machineIdentityKey, shellQuote(installedPath), shellQuote(installedPath))
}

func shellQuote(path string) string { return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'" }
func runtimeArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

func bootstrapReceiverScript(stage, final string) string {
	return fmt.Sprintf(`set -eu
umask 077
stage=%s
lock=%s
final=%s
ok="$stage.ok"
mkdir "$lock"
trap 'rm -f "$stage" "$ok"; rmdir "$lock"' EXIT HUP INT TERM
IFS= read -r header
case "$header" in s2a1:*:*) ;; *) exit 64 ;; esac
body=${header#s2a1:}
size=${body%%%%:*}
digest=${body#*:}
case "$size" in ''|*[!0-9]*) exit 64 ;; esac
case "$digest" in *[!0123456789abcdef]*|?????????????????????????????????????????????????????????????????) exit 64 ;; esac
[ ${#digest} -eq 64 ]
[ "$size" -le 67108864 ]
dd of="$stage" bs=1 count="$size" status=none
[ "$(wc -c < "$stage")" -eq "$size" ]
[ "$(sha256sum "$stage" | awk '{print $1}')" = "$digest" ]
chmod 700 "$stage"
if [ -L "$final" ]; then exit 64; fi
if [ -e "$final" ] && [ ! -f "$final" ]; then exit 64; fi
set +e
"$stage" bootstrap-stdio 3>"$ok"
status=$?
set -e
if [ -s "$ok" ]; then
  if ! printf %%s 'sub2api-bootstrap-attested-v1' | cmp -s "$ok" -; then exit 64; fi
  [ "$status" -eq 0 ] || exit 64
else
  exit 0
fi
[ "$(wc -c < "$stage")" -eq "$size" ]
[ "$(sha256sum "$stage" | awk '{print $1}')" = "$digest" ]
if [ -L "$final" ]; then exit 64; fi
if [ -e "$final" ] && [ ! -f "$final" ]; then exit 64; fi
mv -T -- "$stage" "$final"
`, shellQuote(stage), shellQuote(stage+".lock"), shellQuote(final))
}

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

type Command uint8

const (
	Probe Command = iota + 1
	BootstrapReceiver
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

func (t Transport) Probe(ctx context.Context, alias string) (artifact.ProbeInfo, error) {
	if err := sshcheck.ValidateAlias(alias); err != nil {
		return artifact.ProbeInfo{}, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if t.start == nil {
		t.start = systemStart
	}
	r := t.start(ctx, "ssh", sshArgs(alias, Probe, probeCommand), nil)
	if r.err != nil {
		return artifact.ProbeInfo{}, processFailure(r)
	}
	parts := strings.Split(string(r.stdout), "\n")
	if len(parts) != 5 {
		return artifact.ProbeInfo{}, ErrProtocol
	}
	if parts[0] != "s2p1:Linux" {
		return artifact.ProbeInfo{}, ErrProtocol
	}
	if parts[1] != "amd64" && parts[1] != "arm64" {
		return artifact.ProbeInfo{}, ErrProtocol
	}
	if !hex64(parts[2]) {
		return artifact.ProbeInfo{}, ErrProtocol
	}
	if parts[3] != "missing" && !hex64(parts[3]) {
		return artifact.ProbeInfo{}, ErrProtocol
	}
	if parts[4] != "" {
		return artifact.ProbeInfo{}, ErrProtocol
	}
	return artifact.ProbeInfo{OS: "Linux", Arch: parts[1], Machine: "mid1:" + parts[2], InstalledDigest: parts[3]}, nil
}
func hex64(v string) bool { return regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(v) }
func (t Transport) Bootstrap(ctx context.Context, alias string, stdin []byte) (hostprotocol.Response, error) {
	return t.Run(ctx, alias, BootstrapReceiver, stdin)
}
func sshArgs(alias string, _ Command, remote string) []string {
	return append([]string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "--", alias}, remote)
}
func remoteCommand(c Command) (string, bool) {
	switch c {
	case Probe:
		return probeCommand, true
	case BootstrapReceiver:
		return "sudo -n /bin/sh -c " + shellQuote(bootstrapReceiverScript(stagePath, finalPath)) + " fixed-argv0", true
	case Host:
		return finalPath + " stdio", true
	}
	return "", false
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
