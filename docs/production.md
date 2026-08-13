# Production control plane on GKE

The default overlay is intentionally an evaluation/development shape: one API
replica and an in-memory store unless configured otherwise. The
`config/production-gcp` overlay is the production control-plane shape.

It adds:

- two API replicas using the Postgres store;
- Cloud SQL Auth Proxy as a localhost sidecar with automatic IAM database auth;
- database-aware `/readyz` probes;
- two leader-elected operator replicas;
- topology spreading and one-replica disruption budgets.

## Required infrastructure contract

Provision a highly available Cloud SQL for PostgreSQL instance and database,
then grant the `wren-apiserver` Google service account Cloud SQL Client access
and database login. Bind the Kubernetes service account through GKE Workload
Identity and annotate it with its Google service account:

```sh
kubectl annotate serviceaccount -n wren-system wren-apiserver \
  iam.gke.io/gcp-service-account=YOUR_GSA@YOUR_PROJECT.iam.gserviceaccount.com
```

Create the connection secret. `database-url` connects to the proxy on localhost;
URL-encode the IAM database username where required by a PostgreSQL URI.

```sh
kubectl create secret generic wren-postgres -n wren-system \
  --from-literal=instance-connection-name=PROJECT:REGION:INSTANCE \
  --from-literal=database-url='postgresql://IAM_DATABASE_USER@127.0.0.1:5432/wren?sslmode=disable'
```

Apply the overlay after overriding the three Wren image references for your
registry:

```sh
kubectl apply -k config/production-gcp
kubectl rollout status -n wren-system deploy/wren-apiserver
kubectl get -n wren-system poddisruptionbudgets
```

Every API replica runs embedded forward-only migrations on startup. A Postgres
advisory lock serializes a concurrent fresh deployment, and readiness stays
false until the database answers. Launch intents use leased workers, so a pod
dying after commit is recovered by another replica.

Run `make e2e-control-plane-recovery` before promotion to exercise the same
multi-replica migration, full-process replacement, outbox replay, and journal
path locally. Managed creation/deletion of the Cloud SQL instance is not yet a
Wren product command; infrastructure lifecycle remains with the platform's
Terraform or equivalent.
