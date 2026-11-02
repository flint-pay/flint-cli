# Flint CLI 0.16.0

## Changes

- Add `gcp`, `azure`, and `aws` workload identity sources. Auto discovery tries cloud sources only when their platform markers are present. Explicit selection also works on plain VMs.
- Protect cloud identity and AWS credential requests against environment proxies and redirects. AWS uses the SDK credential chain and refuses inbound web identity token files.
- Allow `FLINT_OIDC_SOURCE=kubernetes` without `KUBERNETES_SERVICE_HOST`; auto discovery still requires the marker.
- Link GitHub Actions users to the setup-flint action in the README.
- Fix signup's initial sandbox API key creation by sending the required key name, including on the compatibility fallback.

## Installation and migration

This release adds compatible features and fixes; no migration is required from 0.15.0.

```sh
npm install -g @flintpay/cli@latest
# Or:
brew upgrade flint
flint version --output json
```

See the [README workload identity guide](../README.md#workload-identity-and-automation) for source configuration. Upgrades from 0.14.x should also follow the [0.15.0 migration notes](migrating-0.15.md).
