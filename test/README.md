# Test strategy

- **Unit**: `internal/ipam` (allocation, reservations, reuse, ledger restore)
  and `internal/cloudinit` (render + gzip/base64 round-trip).
- **envtest**: reconciler against a real API server with a fake
  `cloud.Provider` and `kube.Provider`; assert scale-up/scale-down decisions.
- **e2e (dev tenant)**: create a `K8sWorkerPool`, scale 2 -> 4 -> 2 via the CRD,
  verify VMs, Node readiness, LB pool membership ordering, and ledger state.
