//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Agent sandbox autonomy suite — kyber#78 AC1–AC2.
//
// The other half of the bargain. Isolation that costs the agent its ability to
// work is not a win, and the whole reason kyber#77 could not enable user
// namespaces was that doing so broke `apt`. These tests exist to make that
// regression impossible to ship again.
//
// Everything runs through execInSandbox, i.e. inside the agent's chroot. Plain
// `kubectl exec` lands in the base image instead, where an installed package
// would appear to be missing.

const (
	// The package kyber#77 named as the failing case. figlet's postinst does a
	// directory replacement under /etc that overlayfs rejected with EXDEV
	// inside a user namespace, which is what forced the autonomy/isolation
	// tradeoff. It works on an ordinary filesystem.
	canaryPackage = "figlet"
	canaryBinary  = "figlet"

	aptTimeout = 10 * time.Minute
)

// AC1: the agent can install, run, upgrade and remove arbitrary software.
func TestSandboxAutonomy_PackageManagement(t *testing.T) {
	agent := requireAgent(t, "KYBER_E2E_AGENT_A")

	t.Run("apt_install_the_case_that_used_to_fail", func(t *testing.T) {
		out := mustExecInSandbox(t, agent, fmt.Sprintf(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get update -qq >/dev/null 2>&1
			if apt-get install -y %s >/tmp/apt-install.log 2>&1; then
			  echo INSTALLED
			else
			  echo FAILED; tail -20 /tmp/apt-install.log
			fi`, canaryPackage))
		if !strings.Contains(out, "INSTALLED") {
			t.Fatalf("apt install %s failed — this is the exact regression kyber#78 exists to "+
				"prevent:\n%s", canaryPackage, out)
		}
	})

	t.Run("installed_binary_runs", func(t *testing.T) {
		out := mustExecInSandbox(t, agent, canaryBinary+" -w 40 ok 2>&1 | head -3")
		if strings.TrimSpace(out) == "" {
			t.Errorf("%s installed but produced no output", canaryBinary)
		}
	})

	t.Run("apt_remove", func(t *testing.T) {
		// Asserted through dpkg, not `command -v`. figlet registers itself with
		// update-alternatives, which leaves /usr/bin/figlet as a dangling
		// symlink after removal — so a PATH lookup still "finds" a package that
		// is gone, and the first version of this test failed a working uninstall.
		out := mustExecInSandbox(t, agent, fmt.Sprintf(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get remove -y %s >/dev/null 2>&1
			if dpkg -s %s 2>/dev/null | grep -q "^Status: install ok installed"; then
			  echo FAILED; else echo REMOVED; fi`, canaryPackage, canaryPackage))
		if !strings.Contains(out, "REMOVED") {
			t.Error("apt remove did not take effect — AC1 requires uninstall, not just install")
		}
		// Put it back; the persistence test below needs something to persist.
		// The index refresh is required, not belt-and-braces: this image clears
		// /var/lib/apt/lists after an install, so a second apt-get install with
		// no update in between fails with "Unable to locate package".
		mustExecInSandbox(t, agent, fmt.Sprintf(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get update -qq >/dev/null 2>&1
			apt-get install -y %s >/dev/null 2>&1; echo done`, canaryPackage))
	})
}

// AC1: language toolchains and native compilation.
func TestSandboxAutonomy_Toolchains(t *testing.T) {
	agent := requireAgent(t, "KYBER_E2E_AGENT_A")

	t.Run("compile_and_run_native_code", func(t *testing.T) {
		out := mustExecInSandbox(t, agent, `
			export DEBIAN_FRONTEND=noninteractive
			if ! command -v cc >/dev/null 2>&1; then
			  apt-get update -qq >/dev/null 2>&1
			  apt-get install -y build-essential >/dev/null 2>&1
			fi
			d=$(mktemp -d)
			printf '#include <stdio.h>\nint main(void){puts("COMPILED");return 0;}\n' > "$d/t.c"
			cc -o "$d/t" "$d/t.c" 2>/dev/null && "$d/t" || echo FAILED`)
		if !strings.Contains(out, "COMPILED") {
			t.Errorf("could not compile and run native code: %s", out)
		}
	})

	t.Run("npm_global_install", func(t *testing.T) {
		// Verified with `npm ls -g`, not by requiring the module: a global
		// install is not on NODE_PATH for an arbitrary script, so `require`
		// fails for a package that installed perfectly well. That is a fact
		// about node's resolution, not about the sandbox.
		out := mustExecInSandbox(t, agent, `
			if ! command -v npm >/dev/null 2>&1; then echo SKIP; exit 0; fi
			npm install -g --silent left-pad >/dev/null 2>&1 \
			  && npm ls -g --depth=0 2>/dev/null | grep -q left-pad \
			  && echo NPM_OK || echo FAILED`)
		if strings.Contains(out, "SKIP") {
			t.Skip("no npm in this image")
		}
		if !strings.Contains(out, "NPM_OK") {
			t.Errorf("global npm install did not work: %s", out)
		}
	})

	t.Run("pip_install", func(t *testing.T) {
		out := mustExecInSandbox(t, agent, `
			if ! command -v pip3 >/dev/null 2>&1; then echo SKIP; exit 0; fi
			pip3 install --quiet --break-system-packages six >/dev/null 2>&1 \
			  && python3 -c 'import six; print("PIP_OK")' 2>/dev/null || echo FAILED`)
		if strings.Contains(out, "SKIP") {
			t.Skip("no python in this image")
		}
		if !strings.Contains(out, "PIP_OK") {
			t.Errorf("pip install did not work: %s", out)
		}
	})
}

// AC1: ordinary root administration inside the sandbox.
func TestSandboxAutonomy_SystemAdministration(t *testing.T) {
	agent := requireAgent(t, "KYBER_E2E_AGENT_A")

	t.Run("create_user_and_group", func(t *testing.T) {
		out := mustExecInSandbox(t, agent, `
			userdel -r kybertest 2>/dev/null; groupdel kybertest 2>/dev/null
			groupadd kybertest 2>/dev/null && useradd -g kybertest -m kybertest 2>/dev/null \
			  && id kybertest >/dev/null 2>&1 && echo USER_OK || echo FAILED`)
		if !strings.Contains(out, "USER_OK") {
			t.Errorf("could not create a user: %s", out)
		}
	})

	t.Run("modify_system_directories", func(t *testing.T) {
		out := mustExecInSandbox(t, agent, `
			echo "kyber78" > /etc/kyber-autonomy-probe \
			  && install -m 0755 /dev/null /usr/local/bin/kyber-probe \
			  && printf '#!/bin/sh\necho PROBE_OK\n' > /usr/local/bin/kyber-probe \
			  && chmod +x /usr/local/bin/kyber-probe \
			  && /usr/local/bin/kyber-probe || echo FAILED`)
		if !strings.Contains(out, "PROBE_OK") {
			t.Errorf("could not write to /etc and /usr: %s", out)
		}
	})

	t.Run("bind_a_port_and_run_a_background_service", func(t *testing.T) {
		out := mustExecInSandbox(t, agent, `
			(python3 -m http.server 18099 >/dev/null 2>&1 &) 2>/dev/null || \
			  (nohup busybox httpd -f -p 18099 -h /tmp >/dev/null 2>&1 &)
			sleep 3
			if timeout 4 bash -c 'echo > /dev/tcp/127.0.0.1/18099' 2>/dev/null; then echo BOUND; else echo FAILED; fi`)
		if !strings.Contains(out, "BOUND") {
			t.Errorf("could not run a background service on a local port: %s", out)
		}
	})
}

// AC2: it all survives losing the pod.
//
// The criterion also names node loss and machine replacement. On `local-path`
// volumes those cannot pass — the PVC is node-local disk with hard node
// affinity — and that is a storage-class property, not something this suite can
// paper over. It is documented in ADR 0003 rather than asserted here.
func TestSandboxAutonomy_SurvivesPodRecreation(t *testing.T) {
	agent := requireAgent(t, "KYBER_E2E_AGENT_A")

	marker := fmt.Sprintf("kyber78-%d", time.Now().UnixNano())

	// Lay down one of each thing an agent cares about: a package, a system
	// file, and a binary on PATH.
	setup := mustExecInSandbox(t, agent, fmt.Sprintf(`
		export DEBIAN_FRONTEND=noninteractive
		if ! command -v %s >/dev/null 2>&1; then
		  apt-get update -qq >/dev/null 2>&1
		  apt-get install -y %s >/dev/null 2>&1
		fi
		echo %s > /etc/kyber-persistence-probe
		printf '#!/bin/sh\necho %s\n' > /usr/local/bin/kyber-persist-probe
		chmod +x /usr/local/bin/kyber-persist-probe
		command -v %s >/dev/null 2>&1 && [ -x /usr/local/bin/kyber-persist-probe ] && echo READY || echo FAILED`,
		canaryBinary, canaryPackage, marker, marker, canaryBinary))
	if !strings.Contains(setup, "READY") {
		t.Fatalf("could not stage the persistence probe: %s", setup)
	}

	restartAgentPod(t, agent, 8*time.Minute)

	t.Run("package_survived", func(t *testing.T) {
		out := mustExecInSandbox(t, agent,
			fmt.Sprintf(`command -v %s >/dev/null 2>&1 && %s -w 20 ok >/dev/null 2>&1 && echo SURVIVED || echo LOST`,
				canaryBinary, canaryBinary))
		if !strings.Contains(out, "SURVIVED") {
			t.Error("an apt-installed package did not survive pod recreation")
		}
	})

	t.Run("etc_edit_survived", func(t *testing.T) {
		out := mustExecInSandbox(t, agent, `cat /etc/kyber-persistence-probe 2>/dev/null || echo LOST`)
		if !strings.Contains(out, marker) {
			t.Errorf("an /etc edit did not survive pod recreation (got %q, want %q)", out, marker)
		}
	})

	t.Run("usr_local_binary_survived", func(t *testing.T) {
		out := mustExecInSandbox(t, agent, `/usr/local/bin/kyber-persist-probe 2>/dev/null || echo LOST`)
		if !strings.Contains(out, marker) {
			t.Error("a binary written to /usr/local/bin did not survive pod recreation")
		}
	})

	t.Run("boot_metadata_reports_a_durable_root", func(t *testing.T) {
		out, err := execInContainer(t, agent, `cat /persist/kyber/boot-metadata.json`)
		if err != nil {
			t.Fatalf("reading boot metadata: %v", err)
		}
		// After a restart on an already-seeded volume the mode is plain
		// "rootfs" — a re-seed would mean the previous root was lost.
		if !strings.Contains(out, `"mode": "rootfs"`) {
			t.Errorf("expected the agent to reuse its existing durable root, got: %s", out)
		}
	})

	// Leave the agent as we found it.
	t.Cleanup(func() {
		_, _ = execInSandbox(t, agent, `rm -f /etc/kyber-persistence-probe /usr/local/bin/kyber-persist-probe`)
	})
}
