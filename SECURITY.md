# Security

Report a vulnerability using the repository's private security advisory form. Include the affected commit or version, relevant configuration with credentials removed, and enough detail to reproduce the issue. [Report privately](https://github.com/imprun/worker-platform/security/advisories/new)

Worker Platform treats worker manifests, kubeconfig access, and ATE Control credentials as trusted deployment inputs. Keep Gateway and route authoring access restricted to your publishers. Use the Gateway's TLS identity and ATE's sandbox and egress policy to enforce the runtime boundaries. See [ATE deployment](docs/ate-deployment.md).
