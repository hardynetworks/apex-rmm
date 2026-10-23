# VMware vSphere (vCenter) integration

Apex RMM can connect to **vCenter Server** from inside the Apex server container. No extra
software is installed on ESXi (the Apex agent cannot run on ESXi itself). The integration
has two parts, both under **Virtualization** in the dashboard:

1. **Agentless monitoring and control** – inventory of hosts, VMs, datastores and vCenter
   alarms; alerts; VM power actions; one-click link to the VM in the vSphere Client.
2. **Deploy agent to VMs** – installs the Apex agent into running Windows/Linux guests
   through VMware Tools guest operations.

## 1. Requirements

| Need | Detail |
|---|---|
| Network | The Apex server container must reach `https://<vcenter>:443`. |
| vCenter account | A dedicated account (e.g. `svc-apex@vsphere.local`). See privileges below. |
| Public URL | For agent deployment, set **Settings → Public URL** to an address the *VMs* can reach (not `localhost`). |
| Guests (deploy only) | VMware Tools running; a guest admin login (Windows) or root or a user that can use sudo (Linux); guest can reach the Public URL. |

### vCenter privileges

Create a role and assign it to the service account at the vCenter root with *Propagate to children*:

- Monitoring only: **System.Read**, **System.View** (the built-in *Read-only* role is enough).
- Power actions: add **Virtual machine → Interaction → Power on / Power off / Reset / Suspend**.
- Agent deployment: add **Virtual machine → Guest operations → Guest operation modifications,
  Guest operation program execution, Guest operation queries**.
- Editing VMs: add **Virtual machine → Change configuration** (CPU count, memory, add / extend /
  remove disk, modify device settings, rename, set annotation), **Virtual machine → Interaction →
  Configure CD media, Connect devices**, **Virtual machine → Snapshot management** (create, remove,
  revert), **Datastore → Allocate space, Browse datastore** and **Network → Assign network**.

Give monitoring-only access if you do not want Apex to be able to change VMs: the power buttons
will then fail with a clear permission message.

## 2. Add a connection

**Virtualization → Connections → Add** (admin only). Enter the vCenter URL (`vcenter.example.com`
is fine), the account and password, and optionally a default client for new devices.

Press **Test connection**. If vCenter uses a self-signed or private-CA certificate you will see
the certificate's SHA-1 thumbprint; compare it with the one in the vSphere Client and press
**Trust exactly this certificate**. Apex then only talks to a server presenting that exact
certificate. (The *Skip certificate verification* switch exists but is not recommended.)

The password is stored encrypted with the server's app key and is never returned to the browser.

### Alerts

Apex raises, updates and auto-resolves these alerts (they appear in **Alerts** and follow your
webhook/ticket settings):

- vCenter unreachable (after two failed polls)
- ESXi host not responding / disconnected
- Hardware sensors that are not green (fans, PSUs, temperature, storage, ...)
- Datastore inaccessible, or above the warning / critical usage thresholds (default 85 % / 95 %)
- VM snapshots older than N days (default 3, 0 = off)
- Unacknowledged vCenter alarms (can be switched off)

## Host power, temperature and fans

On the **Hosts** tab Apex shows, for each ESXi host that reports it:

- **Power draw (W)** and any configured power cap, from vSphere's real-time `power.power.average` counter. The summary tile adds up all hosts and estimates kWh/day (multiply by your rate for cost).
- **Hottest temperature sensor** and **fan count / slowest fan** from the host's hardware health sensors.
- **Hardware details**: every numeric sensor (temperatures, fans, PSU output, voltages) plus 1 h / 24 h / 7 d / 30 d history charts. History is stored once per poll and kept for 30 days.

Sensors that vSphere flags yellow/red still raise alerts (see Alerts above). Apex does not add its own temperature or wattage thresholds.

What you actually see depends on the server: ESXi only exposes sensors the hardware vendor's drivers/CIM providers publish. Hosts using the generic ESXi image often show power draw but few or no temperature/fan sensors; Dell, HPE and Lenovo vendor images usually show many. Hosts that report neither simply show no power or hardware columns.

## 3. Power actions

Technicians and admins can power on, shut down (guest, needs Tools), restart (guest), power off,
reset and suspend VMs. Every action asks for confirmation and is written to the audit log.

### Editing VMs

**Virtualization → Virtual machines → Edit** (admins; everyone else sees **Details**) opens the VM:

- **Summary** with the power actions.
- **CPU & memory**: vCPUs, cores per socket, memory, VM name and notes. While a VM runs, vCPUs and
  memory can only be *added*, and only if CPU / memory hot add is enabled on it; otherwise Apex
  says to power it off first. Cores per socket and any reduction need the VM powered off.
- **Disks**: grow a disk (never shrink: vSphere can't), add a thin or thick disk on any datastore
  the host sees, remove a disk (optionally deleting its file). Grow the partition inside the guest
  afterwards. Disks can't be grown while the VM has snapshots.
- **Network**: move an adapter to another port group (standard or distributed), connect or
  disconnect it.
- **CD/DVD**: mount an `.iso` from any datastore (Apex searches the datastore for them) or eject.
- **Snapshots**: take (optionally with memory, or quiesced through VMware Tools), revert, delete.

Every change is admin-only, risky ones ask first, and all of them are recorded in the audit log
(`vsphere.vm_edit`).

### VMs that run the Apex agent

When Apex knows a device is a vSphere VM (matched by BIOS UUID / serial number, IP address or host
name, as shown in the VM list's *Apex agent* column), the device page gets a **Virtual machine**
tab with all of the above, and the remote viewer gets a **VM** button in its toolbar, so you can
mount an ISO, take a snapshot before a change or hard-reset a hung guest without leaving the
session. A device that is offline because its VM is powered off says so, with a link to power it
on.

## 4. Console

**Open in vSphere** opens the VM page in your vSphere Client, where you can launch the web
console. An in-browser console inside Apex is **not** implemented.

## 5. Deploy the Apex agent to VMs

1. Open **Virtualization → VMs**, tick the VMs, press **Deploy agent**.
2. Choose the client (and optionally site) the new devices will belong to.
3. Enter guest credentials for Windows and/or Linux VMs.
4. Watch progress under **Agent deployments**; the installer log is available per VM.

How it works: Apex creates a short-lived (2 h) enrollment token limited to the number of VMs,
then uses guest operations to run the normal Apex install script in each guest
(PowerShell on Windows, `sh` on Linux; through `sudo` if the Linux user is not root — the account's password is handed to sudo in a private file that is deleted before the installer starts, never on the command line). The token is
revoked when the batch ends.

Guest credentials are used in memory only. They are never written to the database, the audit
log or the installer logs.
For a Linux account other than root, the password is put in a file in the guest's `/tmp` for a
moment (readable only by that account) so sudo can read it; the install command opens and deletes
it before anything else runs, and Apex deletes it again afterwards in case the command never started.

VMs are skipped (with a reason) when they are templates, powered off, have Tools not running,
run an unsupported OS, or already have an agent (tick *Reinstall* to override). Linked agents
are matched by BIOS UUID, then IP, then a unique hostname.

## 6. Limitations / things to know

- Guest operations need VMware Tools (or open-vm-tools) running and, on Windows, a local or
  domain admin account that is allowed to log on non-interactively. Locked-down images, UAC
  remote-restriction on non-built-in local accounts, or Tools without guest ops enabled will fail;
  the message is shown on the deployment row.
- vCenter 7/8 recommended. Standalone ESXi hosts can be added by URL but give a smaller view
  (no clusters or alarms history).
- Deploy is verified against a vCenter simulator and unit tests; it has not been exercised against
  every guest OS. Try one VM first.
- Not covered: in-browser console, VM creation/clone/delete, snapshot create/revert, vMotion.
