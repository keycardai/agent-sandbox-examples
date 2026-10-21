package sandbox

import (
	"context"
	"strings"
)

// release returns the tag of a pinned image reference, such as v0.2.2 for
// ghcr.io/keycardai/sandbox-base:v0.2.2@sha256:...
func release(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		return ref[i+1:]
	}
	return ref
}

// missingImages returns those of refs Docker does not have. It fails when
// Docker does not answer, instead of calling every image missing.
func (l *Local) missingImages(ctx context.Context, refs []string) ([]string, error) {
	if err := l.checkDocker(ctx); err != nil {
		return nil, err
	}
	var missing []string
	for _, ref := range refs {
		if _, err := l.runTool(ctx, "docker", "image", "inspect", ref); err != nil {
			missing = append(missing, ref)
		}
	}
	return missing, nil
}

// checkImages says which images the sandbox runs. Without
// KEYCARD_LOCAL_IMAGE_TAG, that is the release the charts pin, which the
// cluster pulls from ghcr.io, and the check fails when Docker cannot read
// its manifest there. With it, a k3d cluster runs the images Docker has at that tag, and
// the check fails when one is missing.
func (l *Local) checkImages(ctx context.Context) Check {
	check := Check{Name: "Sandbox images"}
	if tag := l.Settings[KeyImageTag]; tag != "" {
		return l.checkTaggedImages(ctx, check, tag)
	}
	base, err := l.pinnedBase()
	if err != nil {
		check.State, check.Detail = CheckFailed, err.Error()
		return check
	}
	if l.Existing() {
		check.Detail = "your cluster pulls the release " + release(base) + " from ghcr.io/keycardai"
		return check
	}
	check.Detail = "the release " + release(base) + " from ghcr.io/keycardai"
	if _, err := l.runTool(ctx, "docker", "manifest", "inspect", base); err != nil {
		check.State, check.Detail = CheckFailed, firstLine(err)
		check.Fix = "Docker must be able to pull " + release(base) + " from ghcr.io/keycardai. " +
			"Fix what Docker says, then check again."
	}
	return check
}

// checkTaggedImages checks the images at KEYCARD_LOCAL_IMAGE_TAG.
func (l *Local) checkTaggedImages(ctx context.Context, check Check, tag string) Check {
	if l.Existing() {
		check.Detail = "your cluster pulls the images at " + tag + " from ghcr.io/keycardai"
		return check
	}
	missing, err := l.missingImages(ctx, images(tag))
	switch {
	case err != nil:
		check.State, check.Detail = CheckFailed, firstLine(err)
	case len(missing) > 0:
		check.State, check.Detail = CheckFailed, "Docker does not have "+strings.Join(missing, ", ")
		check.Fix = "Build them in the egress-proxy repository and tag them with these names, " +
			"or clear " + KeyImageTag + " to run the release the charts pin."
	default:
		check.Detail = "the images Docker has at " + tag
	}
	return check
}
