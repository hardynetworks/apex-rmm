# TP-Link Tapo smart plugs

Apex reads TP-Link Tapo Wi-Fi plugs directly over your local network, with no TP-Link cloud
in between. Plugs with energy monitoring (P110, P115, P110M used over Wi-Fi…) report watts,
volts, amps and today's/this month's kWh; others (P100, P105) can still be switched.

A plug can be assigned to an agent device or to an ESXi host. Its measured watts then replace
that device's or host's own power reading (the device's Power tab, the dashboard, and
**Virtualization → Energy**), and admins can turn it off, on, or power cycle it.

## What the Apex server needs

- **Network access to the plugs.** The Apex server polls each plug every 10 seconds on port 80
  (some newer firmware uses HTTPS on 4433). The Apex container must be able to reach the plug's
  IP; from Docker's default bridge network it can reach your LAN like any other client.
- **The TP-Link account** the plugs were set up with (the email and password you sign in to the
  Tapo app with). Plugs prove they know it during the local login (KLAP); neither is sent
  over the network. Apex stores the password encrypted with its app key and never shows it again.
- **Third-Party Compatibility** turned on in the Tapo app: **Me → Third-Party Services →
  Third-Party Compatibility**. Since firmware 1.4, plugs only accept the local login Apex uses
  while this is on. TP-Link notes that this lets third-party apps use your account; Apex uses it
  only to log in to the plugs on your network.
- **A fixed IP for each plug.** Give each plug a DHCP reservation on your router so its address
  doesn't change. The Tapo app shows the IP under the plug → ⚙ → **Device Info**.

## Set it up

1. **Settings → Smart plugs → TP-Link Tapo plugs**: enter the TP-Link ID (email) and password,
   then **Save account**.
2. Enter a plug's IP address (and optionally a name) and click **Add Tapo plug**. Apex reads the
   plug once before saving it; if that fails, the message says why (wrong password, Third-Party
   Compatibility off, address not reachable).
3. In the **Smart plugs** table, choose what the plug powers under **Powers**: an ESXi host or a
   device.

## Troubleshooting

| Message | What to do |
|---|---|
| "didn't accept the TP-Link email and password" | Re-enter the account; both are case-sensitive. Plugs that were moved to another account need that account. |
| "refused the local login … Third-Party Compatibility" | Turn it on in the Tapo app (above), wait a minute, then try again. |
| "can't reach the plug" / "didn't answer in time" | Check the IP (it may have changed: add a DHCP reservation) and that the Apex host can reach that network (VLANs, firewall). |
| Offline in the table | The plug hasn't answered for 30 seconds; the reason is shown under its name. |

Power cycling switches the plug off for 10 seconds and back on. It cuts power without shutting
anything down; Apex asks for confirmation and records every switch in the audit log.
