# Matter smart plugs

Apex can read and switch **Matter** smart plugs and assign each one to an agent device or a
VMware ESXi host:

- A plug that **measures power** (Electrical Power Measurement, e.g. TP-Link Kasa KP125M) replaces
  that device's or host's own power reading. It shows as *Matter smart plug* on the Power tab,
  the Power card and Virtualization → Energy, and its kWh count towards the daily totals and costs.
- A plug **without a power meter** (Linkind / AiDot Matter plugs are on/off only) still shows
  whether it is on and lets admins **power cycle** what it feeds, e.g. to hard-restart a frozen
  machine.

## 1. Start the Matter server

Apex talks to the plugs through [matterjs-server](https://github.com/matter-js/matterjs-server),
an open-source Matter controller. It is in `docker-compose.yml` behind a profile:

```
cd /opt/apex-rmm
docker compose --profile matter up -d
```

It needs the host's network (Matter finds devices with mDNS/IPv6), so it runs with
`network_mode: host`. To keep its unauthenticated API off your LAN it listens only on the Docker
bridge gateway `172.17.0.1`, which Apex reaches as `host.docker.internal`.

- If `ip addr show docker0` shows a different address, set `MATTER_LISTEN=<that address>` in `.env`.
- With a host firewall (ufw), allow containers to reach it:
  `sudo ufw allow from 172.16.0.0/12 to any port 5580 proto tcp`
- If the container can't write its data: `docker compose run --rm --user root matter chown -R 1000:1000 /data`.
- The plugs must be on the same LAN (and IPv6 link-local must work) as the Docker host.

## 2. Turn it on in Apex

**Settings → Smart plugs**: tick *Enabled*, keep the address `ws://host.docker.internal:5580/ws`
and save. The status should change to *Connected*.

## 3. Add a plug

Matter devices can belong to several apps at once. Set the plug up in its own app first (for
Linkind plugs the **AiDot** app, or Apple Home / Google Home), then open the plug's settings there
and choose **Turn on pairing mode** (Apple: *Turn On Pairing Mode*; Google: *Linked Matter apps &
services → Link apps & services*). Enter the 11-digit code it shows under **Settings → Smart plugs
→ Add a plug**. Pairing takes up to two minutes; the plug keeps working in the other app.

Pairing a brand-new plug straight from its QR label needs Bluetooth on the Docker host, which this
setup doesn't use, so share it from the app as above.

## 4. Assign it

In the plug list, choose what it powers under **Powers**: an ESXi host or an agent device. One plug
per device or host. The device's Power tab then shows the plug, and admins can turn it on/off or
power cycle it (off for 10 seconds, then on). Every switch is in the audit log.
