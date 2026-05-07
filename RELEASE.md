# Release Process

The `dra-driver-google-tpu` project releases both a container image and a Helm chart as OCI artifacts, both
promoted through the [Kubernetes image promotion pipeline](https://github.com/kubernetes/k8s.io/blob/main/registry.k8s.io/README.md) and this includes:

- Container image: `registry.k8s.io/dra-driver-google/dra-driver-google-tpu`
- Helm chart: `registry.k8s.io/dra-driver-google/charts/dra-driver-google-tpu`

## Prerequisites

Before starting the release, ensure you have the following tools installed:
*   [`helm`](https://helm.sh/docs/intro/install/)
*   [`crane`](https://github.com/google/go-containerregistry/tree/main/cmd/crane) (for retrieving digests)
*   [`release-notes`](https://github.com/kubernetes/release/tree/master/cmd/release-notes) (optional, for generating changelogs)

## Step 1: Proposal & Approval

1.  **Create an Issue**: Open a GitHub issue proposing the new release. Include the target version and a rough changelog since the last release.
2.  **Maintainer Approval**: At least two repository [OWNERS](OWNERS) must review and approve the proposal.

## Step 2: Synchronization & Preparation

Once the release proposal is approved:
1.  **Update Chart Metadata**: Open [deployments/helm/dra-driver-google-tpu/Chart.yaml](deployments/helm/dra-driver-google-tpu/Chart.yaml). Update the `version` and `appVersion` to match your approved release version.
    ```yaml
    version: 0.1.0
    appVersion: "0.1.0"
    ```
2.  **Commit & Merge**: Commit this change and merge it into the `main` branch via the standard Pull Request process. Ensure your local `main` branch is updated and your working tree is clean.

## Step 3: Tag and Push

Once the preparation PR is merged into `main`, a maintainer creates an annotated tag on that commit and pushes it to GitHub:
```bash
git tag -a v0.1.0 -m "Release v0.1.0"
git push <remote> v0.1.0
```

`<remote>` refers to the name of the Git remote for the repo at [https://github.com/kubernetes-sigs/dra-driver-google-tpu], likely either `upstream` or `origin`

## Step 4: Staging via Cloud Build

The pushed tag automatically triggers a Cloud Build job (defined in [cloudbuild.yaml](cloudbuild.yaml)). Logs can be found [here](https://pantheon.corp.google.com/cloud-build/builds?referrer=search&project=k8s-staging-images). This job executes `make release`, which packages, builds, and pushes the artifacts to the staging registry:
*   **Container Image**: `us-central1-docker.pkg.dev/k8s-staging-images/dra-driver-google/dra-driver-google-tpu:v0.1.0`
*   **Helm Chart**: `us-central1-docker.pkg.dev/k8s-staging-images/dra-driver-google/charts/dra-driver-google-tpu:0.1.0`

*(Note: The Helm release automatically strips the leading 'v' from the tag to conform to SemVer).*

## Step 5: Retrieve Digests & Promote to Production

1.  **Verify Staging**: Check the Cloud Build logs to ensure the build finished successfully.
2.  **Retrieve Digests**: Use `crane` to get the immutable SHA256 digests for the staged artifacts:
    ```bash
    crane digest us-central1-docker.pkg.dev/k8s-staging-images/dra-driver-google/dra-driver-google-tpu:v0.1.0
    crane digest us-central1-docker.pkg.dev/k8s-staging-images/dra-driver-google/charts/dra-driver-google-tpu:0.1.0
    ```
3.  **Open Promotion PR**: Open a Pull Request against the [kubernetes/k8s.io](https://github.com/kubernetes/k8s.io) repository. Add these retrieved digests to the appropriate images file (e.g., `registry.k8s.io/images/k8s-staging-images/images.yaml`) to sync them to the production `registry.k8s.io`. ([example](https://github.com/kubernetes/k8s.io/pull/9447))

## Step 6: Verify the Promotion

After the promotion PR is merged and the sync job runs, verify that the multi-architecture artifacts are live and accessible in the production registry:
```bash
docker manifest inspect registry.k8s.io/dra-driver-google/dra-driver-google-tpu:v0.1.0
docker manifest inspect registry.k8s.io/dra-driver-google/charts/dra-driver-google-tpu:0.1.0
```

## Step 7: Publish GitHub Release

1.  **Generate Release Notes**: Use the [release-notes tool](https://github.com/kubernetes/release/blob/master/cmd/release-notes/README.md) to generate release notes from PR descriptions since the previous tag:
    ```bash
    export GITHUB_TOKEN=<your-github-token>
    release-notes \
    --org kubernetes-sigs \
    --repo dra-driver-google-tpu \
    --branch main \
    --start-rev v0.0.0 \
    --end-rev v0.1.0 \
    --output release-notes.md
    ```
2.  **Draft GitHub Release**: Create a new Release in GitHub for the `v0.1.0` tag. Paste the generated release notes into the description.
3.  **Announce**: Share the release announcement in the `#wg-device-management` Kubernetes Slack channel.
