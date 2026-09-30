// Package preflight refuses a rendered sandbox manifest it cannot vouch for.
//
// The lint denies by default. It judges a Pod, a Sandbox and a
// SandboxTemplate, each pinned to one apiVersion. It passes a short
// allowlist of objects that hold no pod as unjudged and lists each one in
// the report. Every other object is refused. It decodes a judged object
// strictly into its API types, case-sensitively as the API server does, so
// a field those types do not hold is refused. A manifest in which nothing
// was judged is refused.
package preflight

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	yamlv2 "go.yaml.in/yaml/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

// Rule names a refusal. The codes are a stable contract that CI and
// operators key on, so add a code rather than reword one.
type Rule string

// The rules the lint enforces.
const (
	// RuleEnvInjection is a SandboxTemplate whose envVarsInjectionPolicy
	// lets a claim set environment variables on the pod's containers.
	RuleEnvInjection Rule = "env_injection"
	// RuleNoPodSpec is a Pod with no spec, or a Sandbox or SandboxTemplate
	// with no spec.podTemplate.spec.
	RuleNoPodSpec Rule = "no_pod_spec"
	// RuleNothingJudged is a manifest that holds no Pod, Sandbox or
	// SandboxTemplate.
	RuleNothingJudged Rule = "nothing_judged"
	// RuleUnknownField is a key the judged object's API type does not hold.
	// A key that differs from a field only in case is one.
	RuleUnknownField Rule = "unknown_field"
	// RuleUnknownKind is an object whose apiVersion and kind the lint
	// neither judges nor passes as unjudged.
	RuleUnknownKind Rule = "unknown_kind"
	// RuleUnreadableDocument is a document that is not YAML or JSON, is not
	// an object, repeats a key, or holds a value its API type cannot take.
	// In a JSON stream it is also a truncated value or trailing bytes.
	RuleUnreadableDocument Rule = "unreadable_document"
	// RuleVolumeClaimTemplates is a volume claim template named after a pod
	// volume, which the controller replaces with the claim, or a
	// volumeClaimTemplatesPolicy that lets a claim add or replace templates.
	RuleVolumeClaimTemplates Rule = "volume_claim_templates"
)

// Refusal is one reason the manifest is refused.
type Refusal struct {
	Rule Rule
	// Where names the document by its position among the non-empty
	// documents, and by kind and name when it has them.
	Where  string
	Detail string
}

// Unjudged is an allowlisted object the lint passes without judging it.
type Unjudged struct {
	Where      string
	APIVersion string
	Kind       string
}

// Report is what the lint found in one manifest.
type Report struct {
	Refusals []Refusal
	Unjudged []Unjudged
}

// OK reports whether the manifest has no refusals.
func (r Report) OK() bool { return len(r.Refusals) == 0 }

// jsonSniffSize is how far into a manifest kubectl looks for the "{" that
// makes it a JSON stream.
const jsonSniffSize = 4096

// Check reads every document of a manifest and judges each one. It picks
// the format as kubectl does. A manifest whose first jsonSniffSize bytes
// start with "{" after whitespace is a stream of JSON values, and any other
// manifest is a stream of YAML documents. It skips a document that holds
// nothing or null.
func Check(manifest []byte) Report {
	var build builder
	if utilyaml.IsJSONBuffer(manifest[:min(len(manifest), jsonSniffSize)]) {
		build.jsonStream(manifest)
	} else {
		build.yamlStream(manifest)
	}
	if build.judged == 0 && len(build.refusals) == 0 {
		build.refuse(RuleNothingJudged, "the manifest", "it holds no Pod, Sandbox or SandboxTemplate")
	}
	return Report{Refusals: build.refusals, Unjudged: build.unjudged}
}

// jsonStream judges every JSON value of the stream in order. It refuses the
// first bytes that are not a whole JSON value, and stops there. It refuses
// a value that repeats a key.
func (b *builder) jsonStream(manifest []byte) {
	decoder := json.NewDecoder(bytes.NewReader(manifest))
	for index := 1; ; {
		var value json.RawMessage
		err := decoder.Decode(&value)
		if errors.Is(err, io.EOF) {
			return
		}
		where := fmt.Sprintf("document %d", index)
		if err != nil {
			b.refuse(RuleUnreadableDocument, where, err.Error())
			return
		}
		if bytes.Equal(value, []byte("null")) {
			continue
		}
		index++
		var parsed any
		if strict, err := kjson.UnmarshalStrict(value, &parsed); err != nil || len(strict) > 0 {
			b.refuse(RuleUnreadableDocument, where, errors.Join(append(strict, err)...).Error())
			continue
		}
		b.document(value, where)
	}
}

// yamlStream splits the stream into documents at "---" lines, as kubectl
// does, and judges each document. It refuses a document that a YAML stream
// parser reads as more than one, because kubectl would drop all but the
// first.
func (b *builder) yamlStream(manifest []byte) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(manifest)))
	for index := 1; ; {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return
		}
		where := fmt.Sprintf("document %d", index)
		if err != nil {
			b.refuse(RuleUnreadableDocument, where, err.Error())
			return
		}
		if err = singleDocument(doc); err != nil {
			b.refuse(RuleUnreadableDocument, where, err.Error())
			index++
			continue
		}
		data, err := yaml.YAMLToJSONStrict(doc)
		if err == nil && bytes.Equal(data, []byte("null")) {
			continue
		}
		index++
		if err != nil {
			b.refuse(RuleUnreadableDocument, where, err.Error())
			continue
		}
		b.document(data, where)
	}
}

var errMoreThanOneDocument = errors.New("it holds more than one YAML document. Separate documents with a --- line")

// singleDocument refuses a document that holds a second YAML document. A
// "..." marker or a bare CR line ending can hide one from the "---" reader.
// NEVER call Decode again after it returns an error, because the decoder
// then panics.
func singleDocument(doc []byte) error {
	decoder := yamlv2.NewDecoder(bytes.NewReader(doc))
	var value any
	if err := decoder.Decode(&value); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("unreadable YAML: %w", err)
	}
	switch err := decoder.Decode(&value); {
	case err == nil:
		return errMoreThanOneDocument
	case !errors.Is(err, io.EOF):
		return fmt.Errorf("content after the first YAML document: %w", err)
	}
	return nil
}

type builder struct {
	judged   int
	refusals []Refusal
	unjudged []Unjudged
}

func (b *builder) refuse(rule Rule, where, detail string) {
	b.refusals = append(b.refusals, Refusal{Rule: rule, Where: where, Detail: detail})
}

// objectType is an apiVersion and kind pair.
type objectType struct{ apiVersion, kind string }

func (b *builder) document(data []byte, where string) {
	var head struct {
		metav1.TypeMeta `json:",inline"`

		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := kjson.UnmarshalCaseSensitivePreserveInts(data, &head); err != nil {
		b.refuse(RuleUnreadableDocument, where, err.Error())
		return
	}
	if head.Metadata.Name != "" {
		where += " (" + head.Kind + "/" + head.Metadata.Name + ")"
	}
	b.judge(objectType{head.APIVersion, head.Kind}, data, where)
}

// judge decodes an object by its type. It judges a Pod, a Sandbox and a
// SandboxTemplate, lists an allowlisted type as unjudged and refuses every
// other type.
func (b *builder) judge(typ objectType, data []byte, where string) {
	switch typ {
	case objectType{"v1", "Pod"}:
		if _, ok := decode[corev1.PodSpec](b, data, where); ok {
			b.judged++
		}
	case objectType{"agents.x-k8s.io/v1beta1", "Sandbox"}:
		if spec, ok := decode[sandboxSpec](b, data, where); ok {
			b.podTemplate(where, spec.PodTemplate, spec.VolumeClaimTemplates)
		}
	case objectType{"extensions.agents.x-k8s.io/v1beta1", "SandboxTemplate"}:
		if spec, ok := decode[sandboxTemplateSpec](b, data, where); ok {
			b.sandboxTemplate(where, spec)
		}
	case objectType{"v1", "Namespace"},
		objectType{"v1", "ServiceAccount"},
		objectType{"v1", "ConfigMap"},
		objectType{"networking.k8s.io/v1", "NetworkPolicy"},
		objectType{"rbac.authorization.k8s.io/v1", "Role"},
		objectType{"rbac.authorization.k8s.io/v1", "RoleBinding"}:
		b.unjudged = append(b.unjudged, Unjudged{Where: where, APIVersion: typ.apiVersion, Kind: typ.kind})
	default:
		b.refuse(RuleUnknownKind, where, fmt.Sprintf(
			"the lint neither judges nor passes kind %q from apiVersion %q", typ.kind, typ.apiVersion,
		))
	}
}

// object is a judged object whose spec has type S. It has no status, so a
// manifest that sets one is refused.
type object[S any] struct {
	metav1.TypeMeta `json:",inline"`

	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     *S                `json:"spec"`
}

// decode reads a judged object strictly and returns its spec.
func decode[S any](build *builder, data []byte, where string) (*S, bool) {
	var obj object[S]
	strict, err := kjson.UnmarshalStrict(data, &obj, kjson.DisallowUnknownFields)
	switch {
	case err != nil:
		build.refuse(RuleUnreadableDocument, where, err.Error())
	case len(strict) > 0:
		build.refuse(RuleUnknownField, where, errors.Join(strict...).Error())
	case obj.Spec == nil:
		build.refuse(RuleNoPodSpec, where, "spec is absent")
	default:
		return obj.Spec, true
	}
	return nil, false
}

// sandboxSpec is the spec of an agents.x-k8s.io/v1beta1 Sandbox in
// agent-sandbox v1.0.4.
type sandboxSpec struct {
	OperatingMode        string                `json:"operatingMode,omitempty"`
	PodTemplate          *podTemplate          `json:"podTemplate"`
	Service              *bool                 `json:"service,omitempty"`
	ShutdownPolicy       string                `json:"shutdownPolicy,omitempty"`
	ShutdownTime         string                `json:"shutdownTime,omitempty"`
	VolumeClaimTemplates []volumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`
}

// sandboxTemplateSpec is the spec of an extensions.agents.x-k8s.io/v1beta1
// SandboxTemplate in agent-sandbox v1.0.4.
type sandboxTemplateSpec struct {
	EnvVarsInjectionPolicy     string                `json:"envVarsInjectionPolicy,omitempty"`
	NetworkPolicy              *sandboxNetworkPolicy `json:"networkPolicy,omitempty"`
	NetworkPolicyManagement    string                `json:"networkPolicyManagement,omitempty"`
	PodTemplate                *podTemplate          `json:"podTemplate"`
	Service                    *bool                 `json:"service,omitempty"`
	VolumeClaimTemplates       []volumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`
	VolumeClaimTemplatesPolicy string                `json:"volumeClaimTemplatesPolicy,omitempty"`
}

type podTemplate struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations,omitempty"`
		Labels      map[string]string `json:"labels,omitempty"`
	} `json:"metadata"`
	Spec *corev1.PodSpec `json:"spec"`
}

type volumeClaimTemplate struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations,omitempty"`
		Labels      map[string]string `json:"labels,omitempty"`
		Name        string            `json:"name,omitempty"`
	} `json:"metadata"`
	Spec corev1.PersistentVolumeClaimSpec `json:"spec"`
}

type sandboxNetworkPolicy struct {
	Egress  []networkingv1.NetworkPolicyEgressRule  `json:"egress,omitempty"`
	Ingress []networkingv1.NetworkPolicyIngressRule `json:"ingress,omitempty"`
}

// policyDisallowed is the one explicit value of envVarsInjectionPolicy and
// volumeClaimTemplatesPolicy the lint passes. It is also the CRD default.
const policyDisallowed = "Disallowed"

func (b *builder) sandboxTemplate(where string, spec *sandboxTemplateSpec) {
	if policy := spec.EnvVarsInjectionPolicy; policy != "" && policy != policyDisallowed {
		b.refuse(RuleEnvInjection, where, fmt.Sprintf(
			"spec.envVarsInjectionPolicy is %q and must be %s or absent", policy, policyDisallowed,
		))
	}
	if policy := spec.VolumeClaimTemplatesPolicy; policy != "" && policy != policyDisallowed {
		b.refuse(RuleVolumeClaimTemplates, where, fmt.Sprintf(
			"spec.volumeClaimTemplatesPolicy is %q and must be %s or absent", policy, policyDisallowed,
		))
	}
	b.podTemplate(where, spec.PodTemplate, spec.VolumeClaimTemplates)
}

// podTemplate judges the pod a Sandbox or SandboxTemplate holds, together
// with the volume claim templates that change its volumes.
func (b *builder) podTemplate(where string, template *podTemplate, claims []volumeClaimTemplate) {
	if template == nil || template.Spec == nil {
		b.refuse(RuleNoPodSpec, where, "spec.podTemplate.spec is absent")
		return
	}
	for i, claim := range claims {
		name := claim.Metadata.Name
		if slices.ContainsFunc(template.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == name }) {
			b.refuse(RuleVolumeClaimTemplates, where, fmt.Sprintf(
				"spec.volumeClaimTemplates[%d] is named %q and replaces the pod volume of that name", i, name,
			))
		}
	}
	b.judged++
}
