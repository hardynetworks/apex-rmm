# Backups to S3 and moving Apex to another server

Apex can back itself up to an S3-compatible bucket and restore from it, either on the
same server or on a brand-new one. A restore brings back everything that lives in the
database:

* devices, with their agent IDs and secrets, so **agents reconnect without being reinstalled**
* users and roles, including local logins and passwords
* settings, including SSO, RustDesk, webhook, smart plug and backup settings
* vSphere connections and their saved passwords
* scripts, schedules, jobs, alerts, alert policies, tickets, the audit log, and metric and energy history

Sign-in sessions are not backed up, so everyone signs in again after a restore.

## Setting it up

1. Create a bucket and an access key that can **list, read, write and delete** objects in it.
   It doesn't need any other permissions. For AWS, a policy on `arn:aws:s3:::<bucket>` (`s3:ListBucket`)
   and `arn:aws:s3:::<bucket>/*` (`s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`) is enough.
2. In Apex, open **Settings → Backups**:
   * **Bucket**: click your provider to fill in the endpoint pattern, then enter the endpoint, bucket,
     region, an optional folder, and the access key. **Test connection** writes, reads and deletes a small test file.
     * AWS S3: `https://s3.<region>.amazonaws.com`
     * Cloudflare R2: `https://<account-id>.r2.cloudflarestorage.com`, region `auto`, path-style on
     * Backblaze B2: `https://s3.<region>.backblazeb2.com`
     * Wasabi: `https://s3.<region>.wasabisys.com`
     * MinIO: your MinIO address, path-style on
   * **Encryption**: a passphrase of at least 12 characters. Backups are encrypted with it before
     they leave the server. **Store it in your password manager.** Without it, no one (including you)
     can restore the backups.
   * **Schedule**: the daily backup time (server time; set `TZ=` in `.env` to change the server's
     time zone) and how many backups to keep. Older ones are deleted from the bucket.
3. **Save**, then **Back up now** to make the first one.

If a scheduled backup fails, the webhook in Settings → General (if set) gets a notification, and the
Backups tab shows the error. A backup missed while the server was off runs as soon as it's back.

Each backup is a single file, `<folder>/apex-backup-<UTC date and time>.tar.gz.age`. It is an
[age](https://age-encryption.org)-encrypted, gzipped tar of one CSV file per table plus the server's app
key. You can open one without Apex: `age -d backup.tar.gz.age > backup.tar.gz` (age asks for the passphrase).

## Restoring on the same server

In **Settings → Backups**, pick a backup, click **Restore**, type `RESTORE` and confirm. Apex downloads and
decrypts the whole backup and checks it before changing anything. The data is then replaced in one database
transaction, so a failed restore leaves everything as it was. When it's done, Apex restarts (Docker's restart
policy brings it back) and you sign in again.

If you changed the passphrase since the backup was made, enter the old one in the restore dialog.

## Moving Apex to a new server (for example a VPS)

1. **Lower the DNS TTL** of your Apex hostname (e.g. `remote.example.com`) a day ahead, so the switch is quick.
2. On the **old** server, open Settings → Backups and click **Back up now**.
3. On the **new** server, install Apex as in the README (`git clone`, `docker compose up -d --build`).
   Use the same version or a newer one.
4. Open the new server's dashboard. In the setup wizard, enter the setup code from
   `docker compose logs apex`, then choose **Restore a backup instead**. Enter the same bucket details,
   click **Find backups**, pick the backup, enter the passphrase and click **Restore**.
5. When it says the restore is done, sign in with your usual account.
6. **Stop the old server** (`docker compose stop`) so the two don't both run against the same agents and the same bucket.
7. **Point DNS at the new server.** Agents keep the address they were installed with and retry every
   few seconds (backing off to once a minute), so they reconnect on their own once the name resolves to the new server.

Things the backup can't carry, which you need to sort out on the new server:

* **Agents installed with an IP address or a LAN-only name** instead of your public hostname. Check `server` in
  `C:\ProgramData\ApexRMM\agent.json` (Windows), `/etc/apex-agent/agent.json` (Linux) or
  `/Library/Application Support/ApexRMM/agent.json` (macOS). If it isn't the public name, reinstall those agents from the new server.
* **Anything on your home network that the server talks to directly:** vCenter/ESXi, Tapo plugs and the Matter
  controller. A VPS can't reach them on its own. Join the VPS to your VPN (for example a NetBird peer with a
  network route to your LAN) so the same addresses work from it. Test with
  `curl -k https://<vcenter>/sdk` on the VPS before switching over. Matter needs mDNS on the plugs'
  network, so keep the Matter controller at home.
* **RustDesk server keys** live in the `apex-rmm_rustdesk` volume, which Apex can't read. Copy it across
  (see "By hand" in the README) or RustDesk clients will warn that the key changed. Open ports
  21115-21119 TCP and 21116 UDP on the new server.
* **Authentik**: the redirect URI stays the same if the hostname does, but the new server must be able to reach
  your Authentik issuer URL.
* **`.env`** settings (if you use one): copy the file across.

### How saved passwords survive a different app key

Each install generates its own app key, which encrypts saved secrets (SSO client secret, vSphere and Tapo
passwords, the S3 secret key and the backup passphrase). The backup includes the old server's key. On restore,
the new server keeps it, encrypted with its own key, as an extra key for opening those secrets. Nothing needs
re-entering, and secrets saved from then on use the new server's key.

## Notes

* Backups and restores run one at a time. Big databases (years of metrics) can take a few minutes.
* The server needs free temporary disk space about the size of one backup.
* A backup from an older Apex restores into a newer one: columns added since then get their defaults.
  A backup from a newer Apex restores into an older one with a warning listing anything that was skipped.
