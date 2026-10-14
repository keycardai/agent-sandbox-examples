package sandbox

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	// allowedURL is an endpoint in sandbox/values.example.yaml without a
	// credential.
	allowedURL = "https://pypi.org/simple/"
	// githubHost and githubUserURL are the endpoint the proxy injects the
	// user's GitHub credential into.
	githubHost    = "api.github.com"
	githubUserURL = "https://" + githubHost + "/user"
	// openRouterKeyURL is OpenRouter's endpoint for the details of the
	// caller's API key.
	openRouterHost   = "openrouter.ai"
	openRouterKeyURL = "https://" + openRouterHost + "/api/v1/key"
	// deniedHost is a host that sandbox/values.example.yaml does not list.
	deniedHost = "example.com"
	// deniedReason is the reason the proxy logs for a host no endpoint lists.
	deniedReason = "no_matching_endpoint"
	// statusMarker separates the body curl prints from the status code.
	statusMarker = "\n__sandbox_status__:"
	// tlsRetries and tlsRetryDelay bound the wait for the agent container
	// to trust the proxy. The pod is Ready when the container starts.
	// sandbox-base installs the proxy's CA after that.
	tlsRetries    = 10
	tlsRetryDelay = 2 * time.Second
	// curlCouldNotResolve, curlSSLConnect and curlPeerFailedCA are curl's
	// exit codes for a failed name lookup, a failed TLS handshake and an
	// untrusted certificate.
	curlCouldNotResolve = 6
	curlSSLConnect      = 35
	curlPeerFailedCA    = 60
	// probePod is the pod the NetworkPolicy check starts in the sandbox
	// namespace. The NetworkPolicy selects every pod there.
	probePod = "keycard-netpol-probe"
	// probeOverrides keeps the probe pod from mounting a ServiceAccount
	// token, and ends it after 120 seconds.
	probeOverrides = `{"spec":{"automountServiceAccountToken":false,"activeDeadlineSeconds":120}}`
	// probeScript connects to $1 up to 10 times, 2 seconds apart, and prints
	// probeMarker and curl's exit code. It stops at the first failed
	// connection. A policy engine can enforce the policy on a new pod a few
	// seconds after the pod starts.
	probeScript = `for i in 1 2 3 4 5 6 7 8 9 10; do
curl -sS -o /dev/null --globoff --noproxy '*' --connect-timeout 5 "http://$1/"; code=$?
[ "$code" -ne 0 ] && break; sleep 2
done
echo ` + probeMarker + `$code`
	// probeName starts each line the NetworkPolicy check prints.
	probeName = "NetworkPolicy: "
	// probeMarker separates curl's output from its exit code.
	probeMarker = "__probe_exit__:"
	// curlConnectFailed and curlTimedOut are curl's exit codes for a
	// connection that was refused or rejected, and a connect timeout.
	curlConnectFailed = 7
	curlTimedOut      = 28
	// proxyLogLines is how many lines of the proxy's log a check reads.
	proxyLogLines = "--tail=500"
	// clockSkew is how far the cluster's clock may run behind this host's.
	// A check reads only the audit records stamped after the checks started,
	// less clockSkew.
	clockSkew = 2 * time.Second
)

var (
	// ErrCheck is a check that did not pass.
	ErrCheck = errors.New("checks failed")
	// ErrTarget is a namespace, pod or user id that is not a Kubernetes name.
	ErrTarget = errors.New("check target")
)

// CheckTarget is the sandbox the checks run against.
type CheckTarget struct {
	Cluster, Namespace, Pod, UserID string
}

// proxyRecord is the part of the proxy's JSON audit record the checks read.
type proxyRecord struct {
	TS          time.Time `json:"ts"`
	Destination struct {
		Host string `json:"host"`
	} `json:"destination"`
	Decision struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
		Error   string `json:"error"`
	} `json:"decision"`
}

// Check runs the end-to-end checks against a running sandbox and prints each
// result. It returns ErrCheck when an automated check fails. It also prints
// two manual checks. The only thing it writes to the cluster is the
// NetworkPolicy check's probe pod, which kubectl deletes when it exits.
//
// The automated checks:
//   - An allowed destination without a credential returns 200.
//   - The credentialed GitHub endpoint returns an account. When
//     KEYCARD_LOCAL_GITHUB_LOGIN is set, it must be that account.
//   - OpenRouter returns the API key's details. This check runs only when
//     the zone-setup values file has an OpenRouter resource.
//   - The proxy blocks a destination that no endpoint lists, and logs the reason.
//   - The pod runs as the ServiceAccount the credential is bound to.
//   - The cluster enforces the sandbox NetworkPolicy.
func (l *Local) Check(ctx context.Context, target CheckTarget) error {
	checks, failures := 0, 0
	report := func(passed bool, line string) {
		mark := "PASS"
		if !passed {
			mark, failures = "FAIL", failures+1
		}
		checks++
		_, _ = fmt.Fprintf(l.Out, "  %s  %s\n", mark, line)
	}
	since := time.Now().Add(-clockSkew)
	report(l.checkAllowed(ctx, target))
	report(l.checkCredentialed(ctx, target, since))
	if l.configured(openRouterAPI) {
		report(l.checkOpenRouter(ctx, target, since))
	}
	report(l.checkDenied(ctx, target, since))
	report(l.checkServiceAccount(ctx, target))
	report(l.checkNetworkPolicy(ctx, target))
	_, _ = fmt.Fprintf(l.Out, `
  MANUAL  Open the zone's event log. Expect one credentials:issue event for
          the credentialed check, with subject %[1]s
          and actor system:serviceaccount:%[2]s:%[1]s.
          Expect no event for the denied check.
  MANUAL  Deploy a sandbox whose proxy configuration names another enrolled
          user. Expect the zone to refuse the exchange with access_denied,
          and the proxy to log exchange_denied for the first credentialed
          request.
`, target.UserID, target.Namespace)
	if failures > 0 {
		return fmt.Errorf("%w: %d of %d", ErrCheck, failures, checks)
	}
	_, _ = fmt.Fprintln(l.Out, "\nevery automated check passed")
	return nil
}

// CheckSandbox runs Check against the sandbox that CheckTargetFor finds.
func (l *Local) CheckSandbox(ctx context.Context, cluster, namespace, pod, userID string) error {
	target, err := l.CheckTargetFor(cluster, namespace, pod, userID)
	if err != nil {
		return fmt.Errorf("finding the sandbox: %w", err)
	}
	return l.Check(ctx, target)
}

// CheckTargetFor returns the sandbox an earlier Run created for
// KEYCARD_LOCAL_USER_EMAIL, from local/out/zone-setup.yaml. A non-empty
// argument overrides the value from the file. The pod name defaults to the
// user id. Each value goes to kubectl as an argument, so CheckTargetFor
// returns ErrTarget unless the namespace and user id are DNS labels and the
// pod name is a DNS subdomain.
func (l *Local) CheckTargetFor(cluster, namespace, pod, userID string) (CheckTarget, error) {
	var err error
	target := CheckTarget{Namespace: namespace, Pod: pod, UserID: userID}
	if target.Cluster, err = l.cluster(cluster); err != nil {
		return target, err
	}
	if target.UserID == "" || target.Namespace == "" {
		out, err := l.readZoneSetup()
		if err != nil {
			return target, err
		}
		access, err := userAccess(out, l.Settings[KeyUserEmail])
		if err != nil {
			return target, err
		}
		target.UserID = cmp.Or(target.UserID, access.UserID)
		target.Namespace = cmp.Or(target.Namespace, out.K8sSandbox.Namespace)
	}
	target.Pod = cmp.Or(target.Pod, target.UserID)
	for _, name := range []struct {
		field, value string
		problems     []string
	}{
		{"namespace", target.Namespace, validation.IsDNS1123Label(target.Namespace)},
		{"user id", target.UserID, validation.IsDNS1123Label(target.UserID)},
		{"pod", target.Pod, validation.IsDNS1123Subdomain(target.Pod)},
	} {
		if len(name.problems) > 0 {
			return target, fmt.Errorf("%w: the %s %q is not valid: %s",
				ErrTarget, name.field, name.value, strings.Join(name.problems, "; "))
		}
	}
	return target, nil
}

func (l *Local) checkAllowed(ctx context.Context, target CheckTarget) (bool, string) {
	status, _, err := l.fetch(
		ctx,
		target,
		"curl",
		"-sS",
		"-o",
		"/dev/null",
		"-w",
		statusMarker+"%{http_code}",
		allowedURL,
	)
	if err != nil {
		return false, fmt.Sprintf("allowed, no credential: %s failed: %v", allowedURL, err)
	}
	if status != http.StatusOK {
		return false, fmt.Sprintf("allowed, no credential: %s returned %d, want 200", allowedURL, status)
	}
	return true, "allowed, no credential: " + allowedURL + " returned 200"
}

// checkCredentialed sends the placeholder in $GITHUB_TOKEN from the agent.
// The proxy replaces it with the credential the zone issued for the
// impersonated user.
func (l *Local) checkCredentialed(ctx context.Context, target CheckTarget, since time.Time) (bool, string) {
	const name = "credentialed: " + githubUserURL
	status, body, err := l.fetch(ctx, target, "sh", "-c",
		`curl -sS -w '`+statusMarker+`%{http_code}' -H "Authorization: Bearer $GITHUB_TOKEN" `+githubUserURL)
	if err != nil {
		return false, fmt.Sprintf("%s failed: %v", name, err)
	}
	if status != http.StatusOK {
		return false, fmt.Sprintf("%s returned %d%s", name, status, l.refusal(ctx, target, since, githubHost))
	}
	var user struct {
		Login string `json:"login"`
	}
	if json.Unmarshal([]byte(body), &user) != nil || user.Login == "" {
		return false, name + " did not return a GitHub account"
	}
	if want := l.Settings[KeyGitHubLogin]; want != "" && user.Login != want {
		return false, fmt.Sprintf("%s named %q, want %q", name, user.Login, want)
	}
	return true, fmt.Sprintf("%s named the GitHub account %q", name, user.Login)
}

// checkOpenRouter has the agent call OpenRouter's key endpoint, which does
// not run a model. The agent sends an empty key. The proxy replaces it with
// the key in the zone's vault.
func (l *Local) checkOpenRouter(ctx context.Context, target CheckTarget, since time.Time) (bool, string) {
	const name = "credentialed: " + openRouterKeyURL
	status, body, err := l.fetch(ctx, target, "sh", "-c",
		`curl -sS -w '`+statusMarker+`%{http_code}' -H "Authorization: Bearer $OPENROUTER_API_KEY" `+openRouterKeyURL)
	if err != nil {
		return false, fmt.Sprintf("%s failed: %v", name, err)
	}
	if status != http.StatusOK {
		return false, fmt.Sprintf("%s returned %d%s", name, status, l.refusal(ctx, target, since, openRouterHost))
	}
	var key struct {
		Data *struct {
			Label string `json:"label"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &key) != nil || key.Data == nil {
		return false, name + " did not return the API key's details"
	}
	return true, fmt.Sprintf("%s returned the API key %q", name, key.Data.Label)
}

// configured reports whether the zone-setup values file has a resource
// for identifier.
func (l *Local) configured(identifier string) bool {
	return slices.ContainsFunc(l.previousValues().values.Resources, func(r zonesetup.ResourceValues) bool {
		return r.Identifier == identifier
	})
}

// checkDenied fetches a destination that no endpoint lists. The proxy's
// resolver answers NXDOMAIN for it, so curl must fail at name resolution.
// The resolver must also log a deny record with deniedReason since the
// checks started.
func (l *Local) checkDenied(ctx context.Context, target CheckTarget, since time.Time) (bool, string) {
	const name = "denied: https://" + deniedHost + "/"
	_, _, err := l.fetch(ctx, target, "curl", "-sS", "-o", "/dev/null", "-w", statusMarker+"%{http_code}",
		"https://"+deniedHost+"/")
	var exit *exec.ExitError
	switch {
	case err == nil:
		return false, name + " was reachable"
	case !errors.As(err, &exit) || exit.ExitCode() != curlCouldNotResolve:
		return false, fmt.Sprintf("%s did not fail at name resolution: %v", name, err)
	}
	_, ok := l.lastDeny(ctx, target, since, func(r proxyRecord) bool {
		return strings.TrimSuffix(r.Destination.Host, ".") == deniedHost && r.Decision.Reason == deniedReason
	})
	if !ok {
		return false, name + " was blocked, but the proxy did not log " + deniedReason + " for it"
	}
	return true, name + " was blocked: " + deniedReason
}

func (l *Local) checkServiceAccount(ctx context.Context, target CheckTarget) (bool, string) {
	out, err := l.kubectl(ctx, target.Cluster, nil, "--namespace", target.Namespace,
		"get", "pod", target.Pod, "-o", "jsonpath={.spec.serviceAccountName}")
	if account := string(out); err == nil && account == target.UserID {
		return true, "the pod runs as the ServiceAccount " + account
	}
	return false, fmt.Sprintf("the pod runs as the ServiceAccount %q, want %q", out, target.UserID)
}

// checkNetworkPolicy starts a pod in the sandbox namespace, from the agent's
// image, that connects to CoreDNS on its liveness probe port. The pod has no
// netsetup or proxy, so only the NetworkPolicy can block it. The policy
// allows only port 53 to CoreDNS. The check fails when the pod connects.
// sandbox does not change the cluster's network setup.
func (l *Local) checkNetworkPolicy(ctx context.Context, target CheckTarget) (bool, string) {
	address, problem := l.coreDNS(ctx, target.Cluster)
	if problem != "" {
		return false, probeName + problem
	}
	image, err := l.kubectl(ctx, target.Cluster, nil, "--namespace", target.Namespace, "get", "pod", target.Pod,
		"-o", `jsonpath={.spec.containers[?(@.name=="agent")].image}`)
	if err != nil || len(image) == 0 {
		return false, fmt.Sprintf("%sreading the agent image of %s failed: %v", probeName, target.Pod, err)
	}
	out, err := l.kubectl(ctx, target.Cluster, nil, "--namespace", target.Namespace, "run", probePod,
		"--image", string(image), "--restart=Never", "--rm", "--stdin", "--quiet",
		"--overrides", probeOverrides, "--command", "--", "sh", "-c", probeScript, "probe", address)
	if err != nil {
		return false, fmt.Sprintf("%sthe probe pod %s failed: %v", probeName, probePod, err)
	}
	return probeResult(target.Namespace, address, out)
}

// coreDNS returns the address and liveness probe port of a running CoreDNS
// pod. CoreDNS answers on that port while the pod runs, so a failed
// connection to it means the NetworkPolicy blocked it. When it cannot find
// them, coreDNS returns why instead.
func (l *Local) coreDNS(ctx context.Context, cluster string) (string, string) {
	out, err := l.kubectl(ctx, cluster, nil, "--namespace", "kube-system", "get", "pods",
		"--selector", "k8s-app=kube-dns", "--field-selector", "status.phase=Running", "-o",
		"jsonpath={.items[0].status.podIP} {.items[0].spec.containers[0].livenessProbe.httpGet.port}")
	if err != nil {
		return "", fmt.Sprintf("finding a running CoreDNS pod in kube-system failed: %v", err)
	}
	ip, port, _ := strings.Cut(string(out), " ")
	addr, ipErr := netip.ParseAddr(ip)
	number, portErr := strconv.ParseUint(port, 10, 16)
	if ipErr != nil || portErr != nil {
		return "", fmt.Sprintf("want the IP and the numeric liveness probe port of a running CoreDNS pod "+
			"in kube-system, got %q", out)
	}
	return netip.AddrPortFrom(addr, uint16(number)).String(), ""
}

// probeResult reads curl's exit code from the probe pod's output. A refused,
// rejected or timed out connection passes.
func probeResult(namespace, address string, out []byte) (bool, string) {
	_, code, _ := strings.Cut(string(out), probeMarker)
	switch exit, err := strconv.Atoi(strings.TrimSpace(code)); {
	case err != nil:
		return false, fmt.Sprintf("%sthe probe pod did not print curl's exit code: %q", probeName, out)
	case exit == curlConnectFailed || exit == curlTimedOut:
		return true, fmt.Sprintf("%sa pod in %s could not connect to CoreDNS at %s (curl exit %d)",
			probeName, namespace, address, exit)
	case exit == 0:
		return false, fmt.Sprintf("%sa pod in %s connected to CoreDNS at %s. "+
			"The cluster does not enforce NetworkPolicy, so only netsetup and the proxy restrict "+
			"the sandbox's egress", probeName, namespace, address)
	default:
		return false, fmt.Sprintf("%sthe probe's curl to %s exited %d, want %d or %d",
			probeName, address, exit, curlConnectFailed, curlTimedOut)
	}
}

// fetch runs a curl command in the agent container that prints the body,
// then statusMarker and the status code. It retries TLS failures until the
// agent trusts the proxy. Any other curl failure is an error.
func (l *Local) fetch(ctx context.Context, target CheckTarget, command ...string) (int, string, error) {
	var out []byte
	var err error
	for range tlsRetries {
		out, err = l.kubectl(ctx, target.Cluster, nil, slices.Concat([]string{
			"--namespace", target.Namespace, "exec", target.Pod, "-c", "agent", "--",
		}, command)...)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || (exit.ExitCode() != curlSSLConnect && exit.ExitCode() != curlPeerFailedCA) {
			break
		}
		select {
		case <-ctx.Done():
			return 0, "", fmt.Errorf("waiting to retry curl: %w", ctx.Err())
		case <-time.After(tlsRetryDelay):
		}
	}
	if err != nil {
		return 0, "", err
	}
	body, status, ok := strings.Cut(string(out), statusMarker)
	code, convErr := strconv.Atoi(strings.TrimSpace(status))
	if !ok || convErr != nil {
		return 0, "", fmt.Errorf("%w: curl did not print a status: %q", ErrCheck, out)
	}
	return code, body, nil
}

// refusal returns why the proxy last refused a request to host, as
// " (proxy: <reason>: <zone error>)", or "" when the log does not have a
// deny record for it.
func (l *Local) refusal(ctx context.Context, target CheckTarget, since time.Time, host string) string {
	record, ok := l.lastDeny(ctx, target, since, func(r proxyRecord) bool {
		return strings.TrimSuffix(r.Destination.Host, ".") == host
	})
	if !ok {
		return ""
	}
	if record.Decision.Error == "" {
		return " (proxy: " + record.Decision.Reason + ")"
	}
	return " (proxy: " + record.Decision.Reason + ": " + record.Decision.Error + ")"
}

// lastDeny returns the last deny record in the proxy's log, stamped at or
// after since, that match accepts.
func (l *Local) lastDeny(
	ctx context.Context,
	target CheckTarget,
	since time.Time,
	match func(proxyRecord) bool,
) (proxyRecord, bool) {
	logs, err := l.kubectl(ctx, target.Cluster, nil, "--namespace", target.Namespace,
		"logs", target.Pod, "-c", "egress-proxy", proxyLogLines)
	if err != nil {
		return proxyRecord{}, false
	}
	lines := strings.Split(strings.TrimSpace(string(logs)), "\n")
	for _, line := range slices.Backward(lines) {
		var record proxyRecord
		if json.Unmarshal([]byte(line), &record) == nil && record.Decision.Verdict == "deny" &&
			!record.TS.Before(since) && match(record) {
			return record, true
		}
	}
	return proxyRecord{}, false
}
