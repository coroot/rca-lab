# Using external databases

By default `make deploy` installs every database the lab needs (operators +
clusters). A database can instead be provided from outside — an existing
cluster, or one managed by a platform such as Percona Everest — and the lab
will neither install nor touch it:

```bash
make deploy EXTERNAL_DBS=mysql
```

Supported today: `mysql`. The setting is recorded in the cluster (ConfigMap
`rca-lab`, namespace `default`), so `make clean` and `make status` honour it
without being told again: `make clean` leaves the external cluster, its
volumes and its operator alone. Switching a database between lab-managed and
external requires a `make clean` first.

Nothing in the lab is reconfigured to point at an external database. The
applications, the seed job, the retention job and the failure scenarios all
reach MySQL through fixed names in namespace `default`, so an external
database is plugged in by **satisfying that contract**, not by editing
manifests.

## MySQL contract

| Object (namespace `default`) | Purpose |
|---|---|
| Service `mysql-haproxy`, port 3306 | Writer endpoint: all application traffic, the seed job, database creation, and the database scenarios' workloads. |
| Service `mysql-haproxy-replicas`, port 3306 | Reader endpoint: the `mysql-retention` CronJob runs its read-only cutoff scans here. May simply point at the writer. |
| Secret `mysql-custom-user-secret`, keys `orders`, `payments` | Passwords of the `orders` and `payments` users (key = username). |
| Secret `mysql-secrets`, key `root` | Root password. Used by the `mysql-init` Job (`CREATE DATABASE IF NOT EXISTS orders, payments`) and the `mysql-retention` CronJob. |
| Secret `mysql-secrets`, key `monitor` | Optional — only needed for Coroot's MySQL scraping (see below). |

`deploy.sh` checks the Services and Secret keys exist before deploying and
stops with a message naming the missing piece.

Users: `orders` with full DML/DDL on database `orders`, and `payments` on
database `payments`, allowed from any host (`'%'`). The reference grants are in
`deploy/databases/mysql.yaml` (`spec.users`). The databases themselves are
created by `mysql-init` as root, so they need not pre-exist.

Pointing the names at a cluster in another namespace is a matter of
`ExternalName` Services, e.g.:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: mysql-haproxy
  namespace: default
spec:
  type: ExternalName
  externalName: mysql-haproxy.everest.svc.cluster.local
```

and copying the two Secrets into `default`.

### Sizing and settings the external cluster should match

The load-generator drives continuous order/payment traffic, so a MySQL that is
not tuned like the lab's one fails in the ways the lab's one used to:

- **Binary log retention**: `binlog_expire_logs_seconds = 3600` and
  `max_binlog_size = 268435456`. MySQL's 30-day default fills a 20 Gi volume in
  about two days at the lab's write rate.
- **Buffer pool**: a pinned `innodb_buffer_pool_size` (the lab uses 1G under a
  3 Gi memory limit). An autotuned 50 %-of-limit pool leaves too little
  headroom for Galera + per-connection buffers and OOM-kills.
- **Connections**: `max_connections` ≥ 170; the apps keep pools open.
- **Volume**: 20 Gi. The `mysql-retention` CronJob (still deployed with an
  external MySQL) bounds row growth to a 7-day window; it assumes Galera
  (`wsrep_flow_control_paused_ns`) for its pacing and is harmless on
  non-Galera MySQL, where that status variable is simply absent.

### Scenarios that need more than the contract

Two reliability scenarios act on the Galera cluster itself, not on the
endpoints, and only work when the external MySQL is a 3-node Percona XtraDB
Cluster named `mysql` in namespace `default` (which is what the Percona
operator — and Everest on top of it — produces for a cluster of that name):

- `sc-19` (certification conflicts) connects straight to two nodes as
  `mysql-pxc-0.mysql-pxc` and `mysql-pxc-1.mysql-pxc`.
- `sc-20` (quorum loss) has Chaos Mesh partition the pod labelled
  `app.kubernetes.io/component=pxc`, `apps.kubernetes.io/pod-index=2`.

With any other external MySQL they fail to start or do nothing; every other
scenario works unchanged.

### Coroot monitoring

The lab's own PXC pods carry `coroot.com/mysql-scrape-*` annotations that make
Coroot scrape each node as the `monitor` user (password in `mysql-secrets`,
key `monitor`). For an external cluster, put equivalent annotations on its
pods (see `deploy/databases/mysql.yaml`) or configure the integration in
Coroot directly.
