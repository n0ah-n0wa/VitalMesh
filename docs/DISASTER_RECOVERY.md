# Disaster recovery

What can be lost, how it comes back, and how long that takes. This is the
document SPECIFICATIONS.md sections 79 and 80 ask for: backups, restore
procedures, retention, recovery objectives, infrastructure recreation,
secret recovery and failure scenarios, with the targets stated rather than
implied. Everything here refers to the Terraform in
`infrastructure/terraform`; nothing in it has been applied yet, and the
restore test at the end (section 80, OQ-32) has therefore not been run.

## What holds state

| Store | Holds | Loss means |
|---|---|---|
| PostgreSQL (RDS) | patients, measurements, jobs, results, users, audit records: every record of truth | data loss; the only store that must be restored |
| Redis (ElastiCache) | rate-limit counters, a short-lived cache, idempotency locks | a window in which a retried request could be processed twice, and rate limits reset; nothing to restore |
| Secrets Manager | the JWT key, the gateway/processor token, the Redis AUTH token, the RDS master password | everyone signed out, the services unable to start until re-read; recoverable for 30 days after deletion in production |
| ECR | the images | rebuilt from the commit; nothing to restore |
| Terraform state | the map of everything above | rebuilt from the bucket's version history |

## Recovery objectives

Targets are stated for production. Staging has no objectives; it is
destroyed and rebuilt.

| Scenario | RPO (data lost) | RTO (time down) | Mechanism |
|---|---|---|---|
| Availability zone impaired | 0 | 1 to 2 minutes for the database; pods reschedule inside the same window | RDS Multi-AZ synchronous standby fails over; Redis primary fails over to its replica; the node group spans three zones; ARC zonal shift can evacuate the zone with one call |
| Database instance lost or corrupted (bad migration, bad data, hardware) | at most 5 minutes | about 60 minutes | point-in-time restore to a new instance from continuous backups (transaction logs are shipped every five minutes), verify, repoint the application |
| Database deleted through Terraform | 0 at the moment of deletion | about 60 minutes | the final snapshot production is required to take; restore from it as below |
| Whole environment lost | at most 5 minutes | about 3 hours | recreate with one apply, then restore the database from the most recent automated backup or snapshot |
| Terraform state damaged | 0 | 15 minutes | restore the previous object version in the state bucket |
| Secret deleted | 0 | 15 minutes | `restore-secret` within the 30-day window; otherwise regenerate (below) |
| Region unavailable | not covered | not covered | see the gap at the end |

The RTO figures assume a person is present with administrator credentials
and the procedures below to hand. **They are estimates.** A restore test has
been run and is recorded at the end of this document, but it exercised
PostgreSQL's own recovery against a local server rather than RDS, so it
validates the procedure and the verification without measuring how long AWS
takes to provision a replacement instance. The RPO figures are RDS's
documented log-shipping cadence, not a measurement. Nothing in this document
claims a recovery guarantee that has been tested unless it says so
explicitly.

## Backups

**Automated backups.** RDS takes a daily snapshot in the backup window
(02:00 to 03:00 UTC) and ships transaction logs to S3 every five minutes.
Together they allow a restore to any second inside the retention window:
14 days in production, 3 in staging (`db_backup_retention_days`; the
platform module refuses fewer than 7 in production). Backups are encrypted
with the environment's data key, as the instance is.

**Final snapshot.** Deleting production's instance takes
`vitalmesh-production-final` (the platform module refuses to skip it) and
keeps it until deleted by hand. Deletion protection has to be turned off in
its own reviewed change before the deletion can happen at all.

**Automated backups after deletion** are kept (`delete_automated_backups =
false`), so the point-in-time window survives the instance.

**Manual snapshots** before anything risky — a major version upgrade, a
migration that rewrites a table — are one command and are kept until
deleted:

```sh
aws rds create-db-snapshot --db-instance-identifier vitalmesh-production \
  --db-snapshot-identifier vitalmesh-production-before-<what>-<date>
```

**Redis** is not backed up (`snapshot_retention_limit = 0`). Everything in
it has a TTL and a documented degradation path (`docs/FAILURE_MODES.md`).

**The encryption key** is the part of a backup that is easy to forget. Every
snapshot is encrypted with the environment's data key
(`alias/vitalmesh-<env>-data`). A destroyed environment schedules that key
for deletion in 30 days; once it is gone, every snapshot encrypted with it
is unreadable forever. Before destroying an environment whose snapshots
matter, either copy the snapshot under a key that stays
(`aws rds copy-db-snapshot --kms-key-id …`) or cancel the key's deletion
(`aws kms cancel-key-deletion`).

## Restore procedures

Every restore creates a **new** instance beside the old one; nothing is
restored in place. That keeps the damaged instance available for
comparison, makes the restore reversible until the application is
repointed, and is the only way RDS can do it anyway.

The restore is a Terraform change, so it is reviewed, recorded and
repeatable: three inputs in the environment's `terraform.tfvars`, applied,
verified, and then reverted once the old instance is gone.

### A. Point in time (bad migration, bad data)

1. Fix the cause first. A restore into a still-running bad migration is a
   second incident.
2. Choose the moment: the last second before the damage, in UTC. If in
   doubt, `aws rds describe-db-instances --db-instance-identifier
   vitalmesh-production --query 'DBInstances[0].LatestRestorableTime'` is
   the newest possible.
3. In `environments/production/terraform.tfvars`:
   ```hcl
   db_instance_suffix          = "-r20260910"
   db_restore_to_point_in_time = {
     source_db_instance_identifier = "vitalmesh-production"
     restore_time                  = "2026-09-10T14:32:00Z"
   }
   ```
   (`use_latest_restorable_time = true` instead of `restore_time` takes the
   newest moment.) The suffix is required: the module refuses a restore
   that would replace the instance it restores from.
4. `terraform plan`: expect the instance, its two log groups, its alarms and
   its event subscription to be **replaced**, and nothing else to change.
   The old instance is destroyed by this apply, after the new one exists;
   in production, deletion protection stops that destroy until it is
   turned off, which is deliberate: turn it off in the same change only
   once the plan shows exactly what was expected.
5. `terraform apply`. RDS creates the new instance from the backups
   (20 to 40 minutes for a database of this size), Terraform then takes the
   final snapshot of the old one and deletes it. The old instance's log
   groups are kept (`skip_destroy`) until their retention expires them.
6. Verify (below). The application is still pointed at the old address
   until step 7, and the old instance's final snapshot exists if the
   restore was wrong.
7. Repoint the application: the `database_address` and
   `database_master_secret_arn` outputs have changed, so run the deployment
   for the environment, which rewrites the Kubernetes Secret from them and
   rolls the pods. The migration Job runs against the restored database and
   is a no-op if the schema is current.
8. Leave the three inputs in `terraform.tfvars`. They describe the instance
   that now exists; removing them would plan a replacement back to an
   empty database. The next restore uses a new suffix.

### B. From a snapshot (a final snapshot, a manual one before an upgrade)

As A, with:

```hcl
db_instance_suffix             = "-r20260910"
db_restore_snapshot_identifier = "vitalmesh-production-final"
```

A snapshot restore carries the database name and master user from the
snapshot; the module leaves `db_name` and `username` unset in that case, as
RDS requires. The master password is managed by RDS on the new instance and
lands in a new secret, whose ARN the outputs report.

### C. Whole environment

1. If bootstrap is intact, skip to 2. Otherwise apply `bootstrap/` first
   (`infrastructure/terraform/README.md`, first-time order); the state
   bucket is versioned and its previous versions are the state history.
2. Apply the environment root as normal. It creates an empty database.
3. Restore into it as A (from the automated backups of the old instance,
   which survive its deletion) or B (from a snapshot), which replaces the
   empty instance with the restored one.
4. Install the platform components, deploy the application, create the DNS
   record: the README's first-time order from step 4.

Secrets are regenerated by the apply (new JWT key, processor token and
Redis token: everyone is signed out) unless the old ones were recovered
first; see below.

### D. Terraform state

The bucket is versioned. List the versions, restore the previous one, and
run `terraform plan` to confirm it matches reality:

```sh
aws s3api list-object-versions --bucket <state bucket> --prefix environments/production/terraform.tfstate
aws s3api get-object --bucket <state bucket> --key environments/production/terraform.tfstate --version-id <id> state.json
aws s3 cp state.json s3://<state bucket>/environments/production/terraform.tfstate
```

`terraform state pull` / `push` do the same through Terraform. The lock
must not be held while doing this.

### E. Secrets

A deleted secret is recoverable for `secret_recovery_window_days` (30 in
production): `aws secretsmanager restore-secret --secret-id
vitalmesh/production/app`. After the window, or when a secret is
compromised, regenerate: increase `secret_version` in the environment root
and apply. That writes a new JWT key, processor token and Redis AUTH token,
signs everyone out, and needs a deployment to carry the new values into
the cluster. The RDS master password is rotated by RDS on its own schedule;
a compromise is handled by `aws rds modify-db-instance
--rotate-master-user-password`.

## Verifying a restore

A restore that is not checked is a guess. After step 5 of A or B, before
repointing anything:

1. **It is the right moment.** Connect to the new instance (through a pod
   in the cluster, or a bastion that does not otherwise exist) as the
   master user, with the password from the new instance's secret, and
   check the newest rows against what was expected:
   ```sql
   SELECT max(received_at) FROM measurements;
   SELECT count(*) FROM patients;
   SELECT version, applied_at FROM schema_migrations ORDER BY applied_at DESC LIMIT 3;
   ```
2. **It is intact.** `SELECT count(*)` on each table against the numbers
   from the last known-good moment (the dashboards keep them), and the
   application's own integrity checks if any exist by then.
3. **The application can use it.** Point a single gateway pod at it
   (`DATABASE_URL` in a copy of the Secret) and run the smoke tests
   against that pod before rolling everyone.

## The restore test (section 80, OQ-32)

`make db-restore-test` performs one against a real PostgreSQL 16 server and
verifies it row by row. It runs on a developer's machine rather than against
staging, because this project has no AWS account to spend; what that buys,
and what it does not, is stated plainly below.

**Run 2026-09-18** — `scripts/db-restore-test.sh`, PostgreSQL 16.15, schema
version 11:

| Phase | Measured |
|---|---|
| Base backup (`pg_basebackup -Xstream -c fast`) | 1.1 s, 46.3 MB |
| Write-ahead log archived | 5 segments, the archiver reported no failures |
| Point-in-time recovery, to **promotion** (writable) | 4.4 s, 2 segments replayed |
| Logical dump (`pg_dump -Fc`) | 0.4 s, 36 KB |
| Logical restore into a fresh database (`pg_restore`) | 0.5 s |

Recovery is timed to **promotion**, not to the first connection that
succeeds: a server replaying WAL accepts connections while it is still
read-only, so "accepting connections" is the wrong milestone to measure or
to wait on. The base backup timing varies with what else the machine is
doing — 1.1 s idle, 6.6 s while a container suite ran beside it — which is
one more reason these numbers are not an RTO.

The shape of the test: create the schema with the real migrator, seed a
known dataset, take a base backup, commit more data, record that instant as
the recovery target, then commit the change that has to be undone. Destroy
the server **and its storage** — only the base backup and the WAL archive
survive, as they would in S3. Restore to the target, and check.

Every item below is an assertion that fails the run if it does not hold:

- the change committed after the target is **absent** — the property a
  point-in-time restore exists for;
- the 4 patients and 80 readings committed before the target are **all
  present**, to the row;
- the schema version is what it was, and not dirty;
- no reading lost its patient;
- the completed processing job and the operator account survived, and no job
  is left in a non-terminal state;
- an out-of-range reading is **still rejected**, and a duplicate external
  reference is **still rejected** — so the constraints, the validation
  trigger and the unique indexes came back with the data. A restore that
  silently dropped them would satisfy every count above, which is why these
  two negative checks are in the test;
- the application itself connects to the restored database and agrees on the
  schema version;
- the logical copy holds the same rows as the restored instance.

### What this does not establish

The timings are PostgreSQL's own, for a 46 MB database on one machine.
**They are not an RTO for production**, and the RTO column above is
unchanged by them. An RDS point-in-time restore adds everything this test
cannot exercise: the restore API, provisioning a new instance — the
dominant term, and the reason the estimate is tens of minutes rather than
seconds — applying the parameter group, and the deployment that repoints the
application at the new endpoint.

The five-minute RPO is likewise a property of RDS, which ships transaction
logs on that cadence. It is AWS's documented behaviour, not something
measured here.

So: the **procedure and its verification are tested**, and the **duration
and the RPO remain assumptions**. A restore run in staging with a real
account should record its numbers in the table above and replace the RTO
column; until then the RTO figures are estimates and are labelled as such.

### What running it taught

Two failure modes surfaced only by doing it, both of which an operator
following the procedure by hand can hit:

- **A recovery target must be an exact, parseable timestamp.** A target
  whose space between the date and the time is lost — easy when copying a
  value through a shell — makes the server refuse to start at all:
  `invalid value for parameter "recovery_target_time"` followed by a FATAL
  configuration error, with no database. Quote the target, and read it back
  from `postgresql.auto.conf` before starting the instance.
- **The write-ahead log segment holding the target has to reach the archive
  before the source is gone.** The test forces a segment switch and waits
  for the archiver to confirm. RDS manages this, but the self-managed and
  logical paths do not: destroying a source whose last segment is still
  unarchived silently costs every transaction in it.

## Failure scenarios not covered

- **Region loss.** Backups stay in the region. Cross-region automated
  backup replication (`aws_db_instance_automated_backups_replication`, with
  a key in the second region) would give a restore point elsewhere at the
  cost of a second copy; it is not built, because nothing else in the
  environment (VPC, cluster, cache, secrets) exists in another region
  either, and a region-level RTO would need all of it. Section 79 asks
  for the target to be stated, and it is: none.
- **Restore under the same identifier.** Not supported by design; the
  suffix is mandatory.
- **The 7-day master password rotation** (README, known gaps) can make
  the *restored* instance's secret differ from what the cluster holds; a
  deployment after a restore is always required, which step 7 says.
