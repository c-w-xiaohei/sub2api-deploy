#!/usr/bin/env python3
"""Isolated behavioral fixtures for the legacy code2 adoption shell entrypoint."""
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/adopt-single-site-layout.sh"


class AdoptionFixture(unittest.TestCase):
    def setUp(self):
        self.work = Path(tempfile.mkdtemp(prefix="legacy-adopt-"))
        self.runtime = self.work / "runtime"
        self.bin = self.work / "bin"
        self.state = self.work / "fake-state.json"
        self.log = self.work / "calls.log"
        self.runtime.mkdir()
        self.bin.mkdir()
        # Execute copied production assets in a disposable working directory.
        shutil.copytree(ROOT / "scripts", self.work / "scripts")
        shutil.copytree(ROOT / "compose", self.work / "compose")
        self.script = self.work / "scripts/adopt-single-site-layout.sh"
        (self.runtime / "deploy-state.json").write_text('{"activeSlot":"blue","postgresMode":"neon","redisMode":"upstash"}\n')
        (self.runtime / "acme.json").write_text("legacy acme\n")
        (self.runtime / "data").mkdir()
        (self.runtime / "data/keep").write_text("never move data\n")
        self.state.write_text(json.dumps({"network": False, "edge": False, "attached": False, "legacy": True}))
        self.install_fakes()
        self.env = os.environ | {
            "PATH": f"{self.bin}:{os.environ['PATH']}", "FAKE_STATE": str(self.state), "FAKE_LOG": str(self.log),
            "TRAEFIK_IMAGE": "traefik:test", "CLOUDFLARE_API_TOKEN": "token", "ACME_EMAIL": "ops@example.test",
            "SING_BOX_SERVER_NAME": "www.cloudflare.com", "SING_BOX_TARGET": "host.docker.internal:8443",
            "DOMAIN": "code2.example.test", "ORIGIN_IP": "203.0.113.10", "APP_PROBE_PATH": "/ready",
            "POSTGRES_MODE": "neon", "REDIS_MODE": "upstash", "SING_BOX_VERIFY_COMMAND": "verify-sing-box",
        }

    def tearDown(self):
        shutil.rmtree(self.work)

    def executable(self, name, body):
        path = self.bin / name
        path.write_text("#!/bin/bash\n" + body)
        path.chmod(0o755)

    def install_fakes(self):
        # The fake owns a small, explicit container/network state model. Unknown
        # invocations fail so a changed shell command cannot be hidden by exit 0.
        self.executable("sub2api-deploy", r'''set -euo pipefail
printf 'RUNTIME %s\n' "$*" >> "$FAKE_LOG"
[[ "$1" == runtime ]] || exit 97
case "$2" in
  read-state) python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$3" "$4" ;;
  has-modes) grep -q postgresMode "$3" ;;
  deployment-mode)
    [[ "$3" == check ]] && exit 0
    [[ "$3" == adopt ]] || exit 98
    printf '{"activeSlot":"blue","postgresMode":"%s","redisMode":"%s"}\n' "$5" "$6" > "$4" ;;
  host-state)
    [[ "${FAIL_AT:-}" != finalhoststate || "$6" != complete ]] || exit 91
    printf '{"sites":["code2"],"handover":"%s"}\n' "$6" > "$4" ;;
  mapping-state)
    [[ -f "$3" ]] || exit 1
    case "$4" in pending) grep -q '"handover":"pending"' "$3" ;; complete) grep -q '"handover":"complete"' "$3" ;; either) grep -q '"handover":"\(pending\|complete\)"' "$3" ;; esac ;;
  edge-env) [[ "${FAIL_AT:-}" != stageenv ]] || exit 91; printf 'TRAEFIK_IMAGE="test"\n' ;;
  dotenv) [[ "${FAIL_AT:-}" != stageenv ]] || exit 91; mkdir -p "$(dirname "$4")"; cat > "$4" ;;
  edge) [[ "${FAIL_AT:-}" != stageenv ]] || exit 91; mkdir -p "$4/dynamic"; printf 'static\n' > "$4/traefik.yml"; printf 'singbox\n' > "$4/dynamic/00-sing-box.yml" ;;
  route) [[ "${FAIL_AT:-}" != route ]] || exit 91; mkdir -p "$(dirname "$5")"; printf 'route\n' > "$5" ;;
  *) exit 98 ;;
esac''')
        self.executable("docker", r'''exec python3 - "$@" <<'PY'
import json,os,sys
from pathlib import Path
a=sys.argv[1:]; p=Path(os.environ['FAKE_STATE']); s=json.loads(p.read_text()); fail=os.environ.get('FAIL_AT',''); log=Path(os.environ['FAKE_LOG'])
log.write_text((log.read_text() if log.exists() else '')+'DOCKER '+' '.join(a)+'\n')
def save(): p.write_text(json.dumps(s))
def labels(i): return {'aaaaaaaaaaaa':'sub2api/traefik','bbbbbbbbbbbb':'sub2api/sub2api-blue','cccccccccccc':'sub2api-edge/traefik'}[i]
if a[0]=='ps':
 x=' '.join(a)
 if 'project=sub2api-edge' in x: print('cccccccccccc' if s['edge'] else '')
 elif 'service=traefik' in x: print('aaaaaaaaaaaa')
 elif 'service=sub2api-blue' in x: print('bbbbbbbbbbbb')
 raise SystemExit
if a[0]=='inspect':
 if len(a)==2: raise SystemExit(0 if a[1] in ('aaaaaaaaaaaa','bbbbbbbbbbbb') or (a[1]=='cccccccccccc' and s['edge']) else 1)
 fmt,i=a[2],a[3]
 if 'Config.Labels' in fmt: print(labels(i),end=''); raise SystemExit
 if '.State.Running' in fmt: print(str(s['legacy'] if i=='aaaaaaaaaaaa' else s['edge']).lower()); raise SystemExit
 if 'NetworkSettings' in fmt: print('net' if s['attached'] else '',end=''); raise SystemExit
 raise SystemExit(94)
if a[:2]==['network','inspect']:
 if not s['network']: raise SystemExit(1)
 if len(a)>3: print(os.environ.get('NETWORK_LABELS','sub2api-edge'),end='')
 raise SystemExit
if a[:2]==['network','connect']:
 if not s['network']: raise SystemExit(95)
 if fail=='attach': raise SystemExit(91)
 s['attached']=True; save(); raise SystemExit
if a[:2]==['network','disconnect']: s['attached']=False; save(); raise SystemExit
if a[:2]==['network','rm']:
 if s['attached']: raise SystemExit(93)
 s['network']=False; save(); raise SystemExit
if a[0]=='compose':
 if a[-2:] == ['create','traefik']:
  s['network']=True; save()
  if fail=='networkcreate': raise SystemExit(91)
  s['edge']=True; save(); raise SystemExit
 if a[-2:] == ['start','traefik']: s['edge']=True; save(); raise SystemExit
 raise SystemExit(94)
if a[0]=='stop': s['legacy' if a[1]=='aaaaaaaaaaaa' else 'edge']=False; save(); raise SystemExit
if a[0]=='start': s['legacy']=True; save(); raise SystemExit
if a[0]=='rm': s['edge']=False; save(); raise SystemExit
if a[0] in ('logs',): raise SystemExit
raise SystemExit(94)
PY''')
        self.executable("ss", "exit 0")
        self.executable("verify-sing-box", "[[ \"${FAIL_AT:-}\" != probe ]]")
        self.executable("bash", r'''if [[ "$1" == scripts/probe-origin-strict.sh || "$1" == scripts/probe-origin.sh ]]; then
  printf 'PROBE %s\n' "$1" >> "$FAKE_LOG"; [[ "${FAIL_AT:-}" == probe ]] && exit 91; exit 0
fi
if [[ "$1" == -c ]]; then printf 'SINGBOX %s\n' "$2" >> "$FAKE_LOG"; fi
exec /bin/bash "$@"''')

    def invoke(self, mode, check=True):
        result = subprocess.run(["/bin/bash", str(self.script), "--environment", "test", "--site", "code2", "--host-state", str(self.runtime / "host-state.json"), mode], cwd=self.work, env=self.env, text=True, capture_output=True)
        if check and result.returncode: self.fail(f"{mode} ({result.returncode}): {result.stderr}\n{result.stdout}\n{self.log.read_text() if self.log.exists() else ''}")
        return result

    def journal(self, state="prepared", **changes):
        r=self.runtime; edge=r/'edge'; fields={"version":"1","environment":"test","site":"code2","host_state":str(r/'host-state.json'),"legacy_project":"sub2api","legacy_traefik":"aaaaaaaaaaaa","active_slot":"blue","active_app":"bbbbbbbbbbbb","edge_project":"sub2api-edge","edge_root":str(edge),"route":str(edge/'dynamic/site-code2.yml'),"route_backup":str(edge/'dynamic/site-code2.yml.before-adoption'),"route_preexisting":"false","route_backup_intent":"false","route_backup_created":"false","route_write_intent":"false","edge_network":"sub2api-edge","network_intent":"false","network_created":"false","attachment_intent":"false","attachment_created":"false","edge_container":"uncreated","edge_container_preexisting":"false","edge_container_intent":"false","edge_dynamic_dir_created":"false","edge_env_intent":"false","edge_env_created":"false","edge_static_intent":"false","edge_static_created":"false","edge_singbox_intent":"false","edge_singbox_created":"false","acme_destination_preexisting":"false","acme_intent":"false","acme_created":"false","legacy_state_backup":str(r/'deploy-state.json.before-adoption'),"legacy_state_backup_intent":"false","legacy_state_backup_created":"false","legacy_state_adopted":"false","state":state}|changes
        (r/'adopt-single-site-layout.journal').write_text(''.join(f'{k}={v}\n' for k,v in fields.items()))

    def test_prepare_preview_is_the_only_non_destructive_preview(self):
        self.invoke('--prepare-preview')
        self.assertTrue((self.runtime/'host-state.json').exists())
        self.assertNotIn('DOCKER', self.log.read_text())
        self.assertEqual((self.runtime/'data/keep').read_text(), 'never move data\n')

    def test_apply_success_journals_real_shell_boundary_calls(self):
        self.invoke('--apply')
        journal=(self.runtime/'adopt-single-site-layout.journal').read_text()
        self.assertIn('state=complete', journal)
        self.assertEqual(json.loads((self.runtime/'host-state.json').read_text())['handover'], 'complete')
        self.assertTrue((self.runtime/'edge/dynamic/site-code2.yml').exists())
        calls=self.log.read_text()
        self.assertIn('DOCKER network connect --alias sub2api-blue sub2api-edge bbbbbbbbbbbb', calls)
        self.assertIn('PROBE scripts/probe-origin-strict.sh', calls)
        self.assertIn('PROBE scripts/probe-origin.sh', calls)
        self.assertIn('SINGBOX verify-sing-box', calls)
        self.assertNotIn('volume', calls)

    def test_intent_failures_restore_and_retry(self):
        for point in ('stageenv','networkcreate','attach','route','probe','finalhoststate'):
            with self.subTest(point=point):
                self.env['FAIL_AT']=point
                failed=self.invoke('--apply', check=False)
                self.assertNotEqual(failed.returncode, 0)
                self.assertFalse((self.runtime/'host-state.json').exists(), f"{point}: {failed.stderr}\n{self.log.read_text()}")
                self.assertEqual(json.loads(self.state.read_text()), {"network":False,"edge":False,"attached":False,"legacy":True})
                self.assertEqual((self.runtime/'data/keep').read_text(), 'never move data\n')
                self.env.pop('FAIL_AT')
                self.invoke('--apply')
                self.assertIn('state=complete', (self.runtime/'adopt-single-site-layout.journal').read_text())
                shutil.rmtree(self.runtime/'edge'); (self.runtime/'host-state.json').unlink(); self.state.write_text(json.dumps({"network":False,"edge":False,"attached":False,"legacy":True})); self.log.unlink(missing_ok=True)

    def test_journal_refuses_unknown_duplicate_malformed_and_changed_ownership(self):
        for contents in ('unknown=x\n', 'version=1\nversion=1\n', 'version\n'):
            with self.subTest(contents=contents):
                (self.runtime/'adopt-single-site-layout.journal').write_text(contents)
                self.assertNotEqual(self.invoke('--rollback', False).returncode, 0)
        self.journal(network_intent='true'); self.state.write_text(json.dumps({"network":True,"edge":False,"attached":False,"legacy":True}))
        self.env['NETWORK_LABELS'] = 'unowned-project'
        self.assertNotEqual(self.invoke('--rollback', False).returncode, 0)

    def test_completed_journal_retires_and_pending_is_refused(self):
        self.journal('complete'); (self.runtime/'host-state.json').write_text('{"sites":["code2"],"handover":"complete"}\n')
        self.invoke('--retire-journal')
        self.assertTrue((self.runtime/'adopt-single-site-layout.journal.retired').exists())
        self.journal('pending'); (self.runtime/'host-state.json').write_text('{"sites":["code2"],"handover":"pending"}\n')
        self.assertNotEqual(self.invoke('--retire-journal', False).returncode, 0)


if __name__ == '__main__': unittest.main(verbosity=2)
