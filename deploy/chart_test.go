// Package deploy_test renders the Helm chart with the helm CLI and asserts on the
// manifests it produces. It skips when helm is not on PATH (GitHub's
// ubuntu-latest runners ship it).
package deploy_test

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const chartDir = "helm/earmark"

// render runs `helm template` on the chart with the given --set overrides and
// returns every rendered document keyed by "<Kind>/<name>".
func render(t *testing.T, sets ...string) map[string]map[string]any {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	args := []string{"template", "earmark", chartDir, "--namespace", "earmark"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	var stderr bytes.Buffer
	cmd := exec.Command("helm", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "helm template: %s", stderr.String())

	docs := map[string]map[string]any{}
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if doc == nil {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		docs[doc["kind"].(string)+"/"+meta["name"].(string)] = doc
	}
	return docs
}

// dig walks nested maps (string keys) and list indexes (int keys).
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			require.Truef(t, ok, "expected map at %v", p)
			v = m[k]
		case int:
			l, ok := v.([]any)
			require.Truef(t, ok, "expected list at %v", p)
			require.Less(t, k, len(l))
			v = l[k]
		}
	}
	return v
}

func cronPodSpec(t *testing.T, cj map[string]any) any {
	return dig(t, cj, "spec", "jobTemplate", "spec", "template", "spec")
}

func TestEvalBackfillDisabledByDefault(t *testing.T) {
	docs := render(t)
	for k := range docs {
		assert.NotContains(t, k, "CronJob/", "no CronJob should render by default")
	}
}

func TestEvalBackfillCronJob(t *testing.T) {
	docs := render(t,
		"evalBackfill.enabled=true",
		"image.tag=9.9.9",
		// A decoupled, gateway-keyed config like production's: the CronJob must
		// carry AI_ENDPOINTS/AI_ROLES and the apiKeyEnv secret too.
		"config.aiEndpoints[0].id=embed",
		"config.aiEndpoints[0].type=embeddings",
		"config.aiEndpoints[0].backend=openai-compat",
		"config.aiEndpoints[0].baseURL=http://gw:4000/v1",
		"config.aiEndpoints[0].model=nomic-embed-text",
		"config.aiEndpoints[0].apiKeyEnv=LITELLM_API_KEY",
		"config.aiEndpoints[1].id=judge",
		"config.aiEndpoints[1].type=chat",
		"config.aiEndpoints[1].backend=openai-compat",
		"config.aiEndpoints[1].baseURL=http://gw:4000/v1",
		"config.aiEndpoints[1].model=some-judge",
		"config.aiEndpoints[1].apiKeyEnv=LITELLM_API_KEY",
		"config.aiRoles.embeddings=embed",
		"config.aiRoles.eval=judge",
		"secrets.aiApiKey.itemPath=vaults/x/items/y",
	)
	cj, ok := docs["CronJob/earmark-eval-backfill"]
	require.True(t, ok, "CronJob earmark-eval-backfill not rendered; got %v", keys(docs))

	spec := dig(t, cj, "spec")
	assert.Equal(t, "17 * * * *", dig(t, spec, "schedule"))
	assert.Equal(t, "Forbid", dig(t, spec, "concurrencyPolicy"))
	assert.Equal(t, 600, dig(t, spec, "startingDeadlineSeconds"))
	assert.Equal(t, 3, dig(t, spec, "successfulJobsHistoryLimit"))
	assert.Equal(t, 3, dig(t, spec, "failedJobsHistoryLimit"))
	assert.Nil(t, dig(t, spec, "suspend"))
	assert.Equal(t, 0, dig(t, spec, "jobTemplate", "spec", "backoffLimit"))
	assert.Equal(t, 3000, dig(t, spec, "jobTemplate", "spec", "activeDeadlineSeconds"))
	assert.Equal(t, 86400, dig(t, spec, "jobTemplate", "spec", "ttlSecondsAfterFinished"))

	pod := cronPodSpec(t, cj)
	assert.Equal(t, "Never", dig(t, pod, "restartPolicy"))
	assert.Equal(t, map[string]any{"kubernetes.io/arch": "amd64"}, dig(t, pod, "nodeSelector"))
	c := dig(t, pod, "containers", 0)
	assert.Equal(t, "ghcr.io/jedwards1230/earmark:9.9.9", dig(t, c, "image"))
	assert.Equal(t,
		[]any{"eval", "--backfill-unevaluated", "--write", "--limit", "25"},
		dig(t, c, "args"))

	// The CronJob must share the Deployments' pod hardening and env verbatim:
	// that lockstep (same AI_ENDPOINTS/AI_ROLES, same secrets) is the point of
	// templating it instead of hand-maintaining a raw manifest.
	ingest := docs["Deployment/earmark-ingest"]
	require.NotNil(t, ingest)
	ipod := dig(t, ingest, "spec", "template", "spec")
	ic := dig(t, ipod, "containers", 0)
	assert.Equal(t, dig(t, ipod, "securityContext"), dig(t, pod, "securityContext"))
	assert.Equal(t, dig(t, ic, "securityContext"), dig(t, c, "securityContext"))
	assert.Equal(t, dig(t, ic, "image"), dig(t, c, "image"))

	var ingestEnv []any
	for _, e := range dig(t, ic, "env").([]any) {
		if e.(map[string]any)["name"] != "INGEST_HTTP_ADDR" {
			ingestEnv = append(ingestEnv, e)
		}
	}
	assert.Equal(t, ingestEnv, dig(t, c, "env"))

	names := map[string]bool{}
	for _, e := range dig(t, c, "env").([]any) {
		names[e.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"DATABASE_URL", "AI_ENDPOINTS", "AI_ROLES", "LITELLM_API_KEY"} {
		assert.Truef(t, names[want], "CronJob env missing %s", want)
	}
}

func TestEvalBackfillOptionalFields(t *testing.T) {
	docs := render(t,
		"evalBackfill.enabled=true",
		"evalBackfill.limit=0",
		"evalBackfill.startingDeadlineSeconds=null",
		"evalBackfill.activeDeadlineSeconds=null",
		"evalBackfill.ttlSecondsAfterFinished=null",
		"evalBackfill.suspend=true",
		"evalBackfill.concurrencyPolicy=Replace",
		"evalBackfill.extraArgs[0]=--write=false",
		"evalBackfill.extraEnv[0].name=EVAL_REASONING_EFFORT",
		"evalBackfill.extraEnv[0].value=omit",
	)
	cj := docs["CronJob/earmark-eval-backfill"]
	require.NotNil(t, cj)
	spec := dig(t, cj, "spec").(map[string]any)
	assert.NotContains(t, spec, "startingDeadlineSeconds")
	assert.Equal(t, true, spec["suspend"])
	assert.Equal(t, "Replace", spec["concurrencyPolicy"])
	job := dig(t, spec, "jobTemplate", "spec").(map[string]any)
	assert.NotContains(t, job, "activeDeadlineSeconds")
	assert.NotContains(t, job, "ttlSecondsAfterFinished")

	// limit 0 = no cap: the flag is omitted (the CLI's own "0 = all").
	c := dig(t, cronPodSpec(t, cj), "containers", 0)
	assert.Equal(t,
		[]any{"eval", "--backfill-unevaluated", "--write", "--write=false"},
		dig(t, c, "args"))

	// extraEnv lands after the shared env.
	env := dig(t, c, "env").([]any)
	assert.Equal(t,
		map[string]any{"name": "EVAL_REASONING_EFFORT", "value": "omit"},
		env[len(env)-1])
}

func TestEvalBackfillSchemaRejectsBadValues(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for _, set := range []string{
		"evalBackfill.limit=-1",
		"evalBackfill.concurrencyPolicy=Sometimes",
		"evalBackfill.backoffLimit=-1",
		"evalBackfill.unknownKey=1",
	} {
		t.Run(set, func(t *testing.T) {
			cmd := exec.Command("helm", "template", "earmark", chartDir,
				"--set", "evalBackfill.enabled=true", "--set", set)
			out, err := cmd.CombinedOutput()
			require.Error(t, err, "expected schema rejection, got:\n%s", out)
			assert.Contains(t, string(out), "evalBackfill")
		})
	}
}

func TestEvalBackfillExtraEnvCollisionFails(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for _, name := range []string{"DATABASE_URL", "LOG_FORMAT", "AI_ROLES"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("helm", "template", "earmark", chartDir,
				"--set", "evalBackfill.enabled=true",
				"--set", "evalBackfill.extraEnv[0].name="+name,
				"--set", "evalBackfill.extraEnv[0].value=x",
				// AI_ROLES is only in commonEnv when aiEndpoints/aiRoles are set.
				"--set", "config.aiEndpoints[0].id=e",
				"--set", "config.aiEndpoints[0].type=embeddings",
				"--set", "config.aiEndpoints[0].backend=ollama",
				"--set", "config.aiEndpoints[0].baseURL=http://o:11434/v1",
				"--set", "config.aiEndpoints[0].model=m",
				"--set", "config.aiRoles.embeddings=e")
			out, err := cmd.CombinedOutput()
			require.Error(t, err, "expected collision failure, got:\n%s", out)
			assert.Contains(t, string(out), "evalBackfill.extraEnv sets \""+name+"\"")
		})
	}
}

// helmInstallNotes renders NOTES.txt via a client-side dry-run install.
func helmInstallNotes(t *testing.T, sets ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	args := []string{"install", "earmark", chartDir, "--namespace", "earmark", "--dry-run=client"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	// Helm 3 still dials the cluster for a client dry-run (Helm 4 does not), and
	// `helm template` never renders NOTES.txt — so without a reachable cluster
	// there is no way to render the notes. Skip rather than fail CI.
	if err != nil && strings.Contains(string(out), "Kubernetes cluster unreachable") {
		t.Skipf("helm install --dry-run needs a cluster with this helm: %s", out)
	}
	require.NoError(t, err, "helm install --dry-run: %s", out)
	return string(out)
}

func TestEvalBackfillInPipelineWarning(t *testing.T) {
	const warn = "WARNING: config.evalInPipeline is also true"
	cases := []struct {
		name       string
		inPipeline string
		want       bool
	}{
		{"decoupled", "false", false},
		{"both judge paths", "true", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := helmInstallNotes(t,
				"evalBackfill.enabled=true",
				"config.evalInPipeline="+tc.inPipeline)
			assert.Contains(t, out, "Eval backfill CronJob")
			if tc.want {
				assert.Contains(t, out, warn)
			} else {
				assert.NotContains(t, out, warn)
			}
		})
	}
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// podSpecs returns every pod spec that renders earmark.commonEnv: both
// Deployments and, when enabled, the evalBackfill CronJob.
func podSpecs(t *testing.T, docs map[string]map[string]any) map[string]any {
	t.Helper()
	out := map[string]any{}
	for k, doc := range docs {
		switch {
		case strings.HasPrefix(k, "Deployment/"):
			out[k] = dig(t, doc, "spec", "template", "spec")
		case strings.HasPrefix(k, "CronJob/"):
			out[k] = cronPodSpec(t, doc)
		}
	}
	return out
}

func envValue(t *testing.T, container any, name string) (string, bool) {
	t.Helper()
	env, _ := dig(t, container, "env").([]any)
	for _, e := range env {
		if m := e.(map[string]any); m["name"] == name {
			v, _ := m["value"].(string)
			return v, true
		}
	}
	return "", false
}

func hasNamed(t *testing.T, v any, name string) map[string]any {
	t.Helper()
	l, _ := v.([]any)
	for _, e := range l {
		if m := e.(map[string]any); m["name"] == name {
			return m
		}
	}
	return nil
}

// TestModelsFileOffByDefault: no config.models → no ConfigMap, no MODELS_FILE,
// no mount anywhere (the pods run with an empty registry).
func TestModelsFileOffByDefault(t *testing.T) {
	docs := render(t, "evalBackfill.enabled=true")
	assert.NotContains(t, keys(docs), "ConfigMap/earmark-models")
	for k, spec := range podSpecs(t, docs) {
		c := dig(t, spec, "containers", 0)
		_, set := envValue(t, c, "MODELS_FILE")
		assert.False(t, set, "%s sets MODELS_FILE without config.models", k)
		assert.Nil(t, hasNamed(t, dig(t, spec, "volumes"), "models"), "%s mounts models without config.models", k)
	}
}

// TestModelsFileWiredToEveryPod: with config.models set, every pod that gets
// MODELS_FILE — the CronJob included, since it judges and so stamps propose
// recipes — mounts the file it names. An env var without the mount fails
// startup closed (CONTRACT §2.18).
func TestModelsFileWiredToEveryPod(t *testing.T) {
	docs := render(t,
		"evalBackfill.enabled=true",
		"config.models.steps.propose.expected_model=anthropic/claude-haiku-4-5-20251001",
		"config.models.steps.propose.revision=r20251001",
	)
	cm, ok := docs["ConfigMap/earmark-models"]
	require.True(t, ok, "models ConfigMap not rendered; got %v", keys(docs))
	var file map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(dig(t, cm, "data", "models.yaml").(string)), &file))
	assert.Equal(t, "anthropic/claude-haiku-4-5-20251001", dig(t, file, "steps", "propose", "expected_model"))

	specs := podSpecs(t, docs)
	require.Len(t, specs, 3, "want both Deployments and the CronJob; got %v", keys(docs))
	for k, spec := range specs {
		c := dig(t, spec, "containers", 0)
		v, set := envValue(t, c, "MODELS_FILE")
		require.True(t, set, "%s has no MODELS_FILE", k)
		assert.Equal(t, "/etc/earmark/models.yaml", v, k)
		m := hasNamed(t, dig(t, c, "volumeMounts"), "models")
		require.NotNil(t, m, "%s sets MODELS_FILE but does not mount it", k)
		assert.Equal(t, "/etc/earmark", m["mountPath"], k)
		assert.Equal(t, true, m["readOnly"], k)
		vol := hasNamed(t, dig(t, spec, "volumes"), "models")
		require.NotNil(t, vol, "%s has no models volume", k)
		assert.Equal(t, "earmark-models", dig(t, vol, "configMap", "name"), k)
	}
}

func TestModelsSchemaRejectsBadValues(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for _, set := range []string{
		"config.models.steps.bogus.alias=x",
		"config.models.steps.propose.unknown=x",
		"config.models.other=x",
	} {
		t.Run(set, func(t *testing.T) {
			out, err := exec.Command("helm", "template", "earmark", chartDir, "--set", set).CombinedOutput()
			require.Error(t, err, "expected schema rejection, got:\n%s", out)
		})
	}
}
