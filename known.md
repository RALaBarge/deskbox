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
