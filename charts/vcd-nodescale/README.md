# vcd-nodescale

Kubernetes worker-node scaling for one platform per installation: vCloud Director
(`platform: vcd`) or vSphere (`platform: vsphere`). The chart installs the
controller and, optionally, its management page.

## Before `helm install`

`platform` is required — an installation never serves both:

```yaml
platform: vcd        # or vsphere
```

The chart creates **no Secrets**. These must exist in the release namespace
before the install:

| Secret (`ui.auth.*`) | content | read by |
| --- | --- | --- |
| `secretName` = `nodescale-ui-auth` | key `key` = `users.yaml`: bcrypt hashes | the page |
| `initialPasswordsSecretName` = `nodescale-ui-initial-passwords` | key `passwords`: the plaintext | the owner, once |

They are a pair — the passwords belong to the hashes — so they are written
together. If `nodescale-ui-auth` already exists, the plaintext is unknown (or was
already deleted); do not create a second one, or its passwords will not match the
hashes.

A platform credential Secret is needed as well: `vcd.existingSecret`
(default `vcd-credentials`) or `vsphere.existingSecret`
(default `vsphere-credentials`), holding the account the controller clones with.

```bash
NS=nodescale-system

# the two accounts, hashes only
cat > users.yaml <<EOF
admins:
  - username: admin
    passwordHash: "$(htpasswd -nB admin | cut -d: -f2)"
viewers:
  - username: viewer
    passwordHash: "$(htpasswd -nB viewer | cut -d: -f2)"
EOF
kubectl -n $NS create secret generic nodescale-ui-auth --from-file=users.yaml=users.yaml

# the same passwords once, for the owner
kubectl -n $NS create secret generic nodescale-ui-initial-passwords \
  --from-literal=passwords="admin: <admin-password>
viewer: <viewer-password>"

# the platform account
kubectl -n $NS create secret generic vcd-credentials \
  --from-literal=url=https://vcd.example.com/api \
  --from-literal=user=svc-node-controller \
  --from-literal=password='***' \
  --from-literal=org=myorg \
  --from-literal=vdc=my-vdc \
  --from-literal=insecure=true
```

Read the passwords once, then delete that Secret whenever you like — nothing
reads it and no upgrade recreates it:

```bash
kubectl -n $NS get secret nodescale-ui-initial-passwords -o jsonpath='{.data.passwords}' | base64 -d
kubectl -n $NS delete secret nodescale-ui-initial-passwords
```

An installation driven by the Terraform + Ansible pair needs none of this: the
role creates both UI Secrets in one step, keeps the passwords root-only on the
control host, and passes `bootstrap.defaults` from the same tfvars.

## Values worth knowing

| value | default | meaning |
| --- | --- | --- |
| `platform` | — | `vcd` or `vsphere`; required |
| `image.repository` / `image.tag` | Docker Hub, chart appVersion | the controller image |
| `bootstrap.defaults` | `{}` | installation-wide guest policy (`ntpServers`, `sysctls`, `kernelModules`, `containerd`, `apt`, `sshAllowedSources`); a pool overrides per field |
| `ui.enabled` | `false` | the management page and its API |
| `ui.auth.secretName` | `nodescale-ui-auth` | the hashes |
| `ui.auth.initialPasswordsSecretName` | `nodescale-ui-initial-passwords` | the plaintext, owner-readable |
| `ui.ingress.enabled` | `true` | publish the page through the cluster ingress on `ui.pathPrefix` |
