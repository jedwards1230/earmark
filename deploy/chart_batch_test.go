package deploy_test

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const batchCronJob = "CronJob/earmark-batch"

func TestBatchCronJobDisabledByDefault(t *testing.T) {
	assert.NotContains(t, keys(render(t)), batchCronJob)
}

func TestBatchCronJob(t *testing.T) {
	docs := render(t,
		"batch.enabled=true",
		"image.tag=9.9.9",
		"batch.timeZone=America/New_York",
		"batch.suspend=true",
		"batch.batchSize=25",
		"batch.maxBatches=4",
		"batch.gpuArbiterUrl=http://192.0.2.10:48750/status",
	)
	cj, ok := docs[batchCronJob]
	require.True(t, ok, "batch CronJob not rendered; got %v", keys(docs))

	spec := dig(t, cj, "spec")
	assert.Equal(t, "0 3 * * *", dig(t, spec, "schedule"))
	assert.Equal(t, "America/New_York", dig(t, spec, "timeZone"))
	assert.Equal(t, "Forbid", dig(t, spec, "concurrencyPolicy"))
	assert.Equal(t, true, dig(t, spec, "suspend"))
	assert.Equal(t, 600, dig(t, spec, "startingDeadlineSeconds"))
	job := dig(t, spec, "jobTemplate", "spec")
	assert.Equal(t, 0, dig(t, job, "backoffLimit"))
	assert.Equal(t, 21600, dig(t, job, "activeDeadlineSeconds"))
	assert.Equal(t, 86400, dig(t, job, "ttlSecondsAfterFinished"))

	pod := cronPodSpec(t, cj)
	assert.Equal(t, "Never", dig(t, pod, "restartPolicy"))
	assert.Equal(t, 60, dig(t, pod, "terminationGracePeriodSeconds"))
	c := dig(t, pod, "containers", 0)
	assert.Equal(t, "ghcr.io/jedwards1230/earmark:9.9.9", dig(t, c, "image"))
	assert.Equal(t, []any{"batch", "--batch-size", "25", "--max-batches", "4"}, dig(t, c, "args"))
	v, set := envValue(t, c, "GPU_ARBITER_URL")
	assert.True(t, set, "GPU_ARBITER_URL not set from batch.gpuArbiterUrl")
	assert.Equal(t, "http://192.0.2.10:48750/status", v)
	_, set = envValue(t, c, "DATABASE_URL")
	assert.True(t, set, "batch CronJob must get the shared env")
	assert.Nil(t, hasNamed(t, dig(t, pod, "volumes"), "books"), "batch never reads audio")

	// Same pod hardening as the Deployments.
	ipod := dig(t, docs["Deployment/earmark-ingest"], "spec", "template", "spec")
	assert.Equal(t, dig(t, ipod, "securityContext"), dig(t, pod, "securityContext"))
	assert.Equal(t, dig(t, ipod, "containers", 0, "securityContext"), dig(t, c, "securityContext"))
}

// Defaults: no timeZone, not suspended, --max-batches omitted (0 = until the
// queue drains), no GPU_ARBITER_URL; extraArgs/extraEnv land last.
func TestBatchCronJobDefaultsAndExtras(t *testing.T) {
	docs := render(t,
		"batch.enabled=true",
		"batch.extraArgs[0]=--poll-interval",
		"batch.extraArgs[1]=30s",
		"batch.extraEnv[0].name=EARMARK_TEST_EXTRA",
		"batch.extraEnv[0].value=yes-please",
		"batch.activeDeadlineSeconds=null",
	)
	cj := docs[batchCronJob]
	require.NotNil(t, cj)
	spec := dig(t, cj, "spec").(map[string]any)
	assert.NotContains(t, spec, "timeZone")
	assert.NotContains(t, spec, "suspend")
	assert.NotContains(t, dig(t, spec, "jobTemplate", "spec").(map[string]any), "activeDeadlineSeconds")
	c := dig(t, cronPodSpec(t, cj), "containers", 0)
	assert.Equal(t, []any{"batch", "--batch-size", "10", "--poll-interval", "30s"}, dig(t, c, "args"))
	_, set := envValue(t, c, "GPU_ARBITER_URL")
	assert.False(t, set)
	env := dig(t, c, "env").([]any)
	assert.Equal(t, map[string]any{"name": "EARMARK_TEST_EXTRA", "value": "yes-please"}, env[len(env)-1])
}

func TestBatchSchemaRejectsBadValues(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for _, set := range []string{
		"batch.batchSize=0",
		"batch.maxBatches=-1",
		"batch.concurrencyPolicy=Allow", // fixed at Forbid: not a value
		"batch.terminationGracePeriodSeconds=5",
		"batch.gpuArbiterUrl=http://192.0.2.10:48750", // must be the /status URL
		"batch.unknownKey=1",
	} {
		t.Run(set, func(t *testing.T) {
			out, err := exec.Command("helm", "template", "earmark", chartDir,
				"--set", "batch.enabled=true", "--set", set).CombinedOutput()
			require.Error(t, err, "expected schema rejection, got:\n%s", out)
			assert.Contains(t, string(out), "batch")
		})
	}
}

func TestBatchExtraEnvCollisionFails(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for _, name := range []string{"DATABASE_URL", "GPU_ARBITER_URL"} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command("helm", "template", "earmark", chartDir,
				"--set", "batch.enabled=true",
				"--set", "batch.extraEnv[0].name="+name,
				"--set", "batch.extraEnv[0].value=x").CombinedOutput()
			require.Error(t, err, "expected collision failure, got:\n%s", out)
			assert.Contains(t, string(out), "batch.extraEnv sets \""+name+"\"")
		})
	}
}
