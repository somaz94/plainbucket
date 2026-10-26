# plainbucket Helm Chart

Deploys [plainbucket](https://github.com/somaz94/plainbucket), a single-node S3-compatible server that stores every object as a plain file on one volume.

<br/>

## Install

```bash
kubectl create secret generic plainbucket-auth \
  --from-literal=accessKey=admin --from-literal=secretKey=change-me

helm repo add plainbucket https://somaz94.github.io/plainbucket/helm-repo
helm install plainbucket plainbucket/plainbucket --set auth.existingSecret=plainbucket-auth
```

The chart is also published as an OCI artifact under `oci://ghcr.io/somaz94/charts`.

<br/>

## What the Chart Assumes

- **One pod.** Object and bucket locks live in the server process, so the Deployment always runs one replica with the `Recreate` strategy, and the schema has no `replicaCount`. Scale up by giving the volume more space, not by adding pods.
- **One volume at `/data`.** By default the chart creates a ReadWriteOnce claim and keeps it on `helm uninstall`; set `persistence.existingClaim` to use your own. With `persistence.enabled=false` objects live in an emptyDir and are lost with the pod.
- **Keys come from Secrets.** `auth.existingSecret` holds the full-access key. `auth.credentialsSecret` holds a list of further keys, each optionally limited to some buckets or to reads, mounted as the server's credentials file. `auth.accessKey` / `auth.secretKey` make the chart create a Secret, which is convenient for trying it out but leaves the key in the release history.
- **Hardened pod.** Non-root UID 65532, read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp, and no service account token.

`values.yaml` documents every setting and `values.schema.json` rejects unknown keys and malformed values. The server settings behind `config.*` are described in [docs/configuration.md](https://github.com/somaz94/plainbucket/blob/main/docs/configuration.md).

<br/>

## Examples

`examples/` holds values files for common setups, each rendered by `make test-helm`:

- `minimal.yaml`: one full-access key and a larger volume.
- `runner-cache.yaml`: a GitLab Runner cache with a key limited to its bucket (see [docs/gitlab-runner.md](https://github.com/somaz94/plainbucket/blob/main/docs/gitlab-runner.md)).
- `static-site.yaml`: a website bucket with index and error documents and CORS, served through an Ingress with virtual-hosted-style hosts. Bucket settings under `config.buckets` are rendered into a ConfigMap passed with `-c`.
- `tls-metrics.yaml`: HTTPS in the pod, a Prometheus Operator `ServiceMonitor`, the alerts and the Grafana dashboard.

<br/>

## Monitoring

With `config.metrics=true` the chart can add, for the Prometheus Operator and Grafana:

- `serviceMonitor`: scrapes `/metrics`.
- `prometheusRule`: alerts when the server is down, answers too many requests with 5xx (`501` for unsupported APIs and `503` in read-only mode aside), refuses writes for disk space (`507`), nears the point where it will, has a bucket close to a `max-size` that refuses writes, or loses bucket event notifications. Each alert can be turned off, and its threshold, `for` and severity set, under `prometheusRule.rules`; `prometheusRule.additionalRules` appends your own. The alerts select series by the job and namespace labels the ServiceMonitor gives them; set `prometheusRule.selector` when they are scraped another way.
- `grafanaDashboard`: a ConfigMap holding the dashboard, labelled for Grafana's dashboard sidecar. The dashboard picks its data source, job and namespace from variables, so one copy covers every release: enable it on one of them. The same JSON (`dashboards/plainbucket.json` in the chart) also imports into a Grafana without the sidecar.

<br/>

## Ingress and Secret Rotation

For uploads through an Ingress, raise the controller's body size limit (for ingress-nginx, `nginx.ingress.kubernetes.io/proxy-body-size: "0"`). With `config.tlsSecret` set as well, the controller must speak HTTPS to the pod (`nginx.ingress.kubernetes.io/backend-protocol: HTTPS`).

Webhook keys for [event notifications](https://github.com/somaz94/plainbucket/blob/main/docs/configuration.md#event-notifications) come from a Secret too: `config.notifySecret` is mounted at `/etc/plainbucket/notify`, and each `config.buckets.<name>.notify[].secret-file` names a file in it.

The server re-reads its credentials file, webhook keys and TLS certificate when they change, so rotating `auth.credentialsSecret`, `config.notifySecret` or `config.tlsSecret` takes effect without a restart once Kubernetes updates the mounted files, usually within a minute or two. `auth.existingSecret` reaches the server as environment variables, which only a pod restart refreshes (`kubectl rollout restart`, or a reloader annotation through `podAnnotations`). Values the chart renders itself restart the pod on `helm upgrade`.
