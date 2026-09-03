# Known issues

## Ubuntu 24.04+: bwrap network sandbox fails silently

Symptom: `bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted` on
any job (even `network:false` ones — the loopback setup runs regardless).

Cause: Ubuntu defaults `kernel.apparmor_restrict_unprivileged_userns=1`.
Ubuntu's `bubblewrap` package ships no AppArmor profile allow-listing
itself, so it loses `CAP_NET_ADMIN` in its own fresh netns. Debian and
Fedora/Bazzite are unaffected — Debian doesn't enable the restriction by
default, Fedora uses SELinux instead of AppArmor.

Fix — add an AppArmor profile for bwrap (same one Ubuntu ships for Flatpak):

```
# /etc/apparmor.d/bwrap
abi <abi/4.0>,
include <tunables/global>

profile bwrap /usr/bin/bwrap flags=(unconfined) {
  userns,
  include if exists <local/bwrap>
}
```

```bash
sudo apparmor_parser -r /etc/apparmor.d/bwrap
```

No deskbox code change needed. Confirmed fixed and re-proven end-to-end
(input gate, retry, sandbox violation, network egress cut, host-privacy) on
Ubuntu 24.04.4 LTS.

## Per-job resource limits (`systemd-run --user --scope`) need a live D-Bus session

Symptom: every job fails with `tool exited with error: Failed to connect to
bus: No medium found`, including jobs that would otherwise succeed cleanly.

Cause: the desk runs each job in its own cgroup scope via `systemd-run
--user --scope` so `DESKBOX_JOB_MEMORY_MAX`/`DESKBOX_JOB_TASKS_MAX` are
actually enforced, not just advisory. That needs a working user D-Bus
session. Confirmed to fail this way even from a proper `systemd --user`
unit on a fresh Ubuntu 24.04 VM, not just a bare `nohup`-over-SSH launch —
the boot-time capability check can pass and then stop being true once the
session it was relying on goes away.

Fix: `loginctl enable-linger <user>` so the session persists independent of
any active login, then restart the desk. No deskbox code change needed for
the fix itself, but the desk does not depend on you catching this: it
detects the bus failure the first time a job hits it and disables the
wrapper for the rest of that process's life instead of failing every future
job — see the README's "Per-job resource limits" section.
