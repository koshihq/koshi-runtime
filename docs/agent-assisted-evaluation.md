# Agent-Assisted Evaluation

This guide describes **human-supervised** evaluation of Koshi driven through a coding
agent — [Codex](https://developers.openai.com/codex/ide),
[Claude Code](https://code.claude.com/docs/en/permission-modes),
[GitHub Copilot CLI](https://docs.github.com/copilot/how-tos/use-copilot-agents/use-copilot-cli),
or [Gemini CLI](https://geminicli.com/docs/cli/plan-mode/). It is IDE-neutral: the same
two prompts work unchanged in all four tools.

The canonical procedures live in [`demo/local/README.md`](../demo/local/README.md) and
[`docs/onboarding.md`](onboarding.md). **Those documents remain authoritative.** This guide
only adds: how to enter a read-only/planning posture in each tool, two reusable prompts,
explicit human-approval gates, the evidence to require, and cleanup expectations. The agent
follows the canonical scripts and docs — it does not invent or rewrite them.

> **Run locally.** Use a machine where Docker and a working kubeconfig actually exist. Most
> cloud/hosted agent environments have neither, so they cannot run the demo or reach your
> cluster — they are unsuitable for this workflow unless their environment explicitly
> provides Docker and cluster access. Keep your tool's normal sandboxing and permission
> prompts **on** throughout.

## How to enter a read-only / planning posture

Open the checked-out repository locally and put the agent in its most conservative posture
before doing anything. Install and authenticate each tool from its official page (linked
below) — those commands are not reproduced here.

| Tool | Conservative posture to start in |
| --- | --- |
| **Codex** | If your Codex surface exposes a **Plan mode**, use it. Otherwise start in the most conservative posture and then request a plan: in the IDE extension select **`Chat`** mode (least autonomous — it does not act without you); on the CLI run under the built-in **`:read-only`** permission profile (e.g. `/permissions` → `:read-only`). Switch to an approval-driven mode only for execution. Plan-mode availability varies by surface/version, but `:read-only` is always available. See [Codex IDE](https://developers.openai.com/codex/ide) and [Codex permissions](https://developers.openai.com/codex/permissions). |
| **Claude Code** | Start in **Plan mode**: `claude --permission-mode plan`, or pick Plan mode in the IDE / cycle with `Shift+Tab`. Plan mode reads only and proposes a plan; you approve into an execution mode. See [permission modes](https://code.claude.com/docs/en/permission-modes). |
| **GitHub Copilot CLI** | Run `copilot` and trust **only** the checked-out repository (choose "Yes, proceed" for the session). Enter **plan mode** with `Shift+Tab` or `/plan`. Keep per-tool approval during execution. See [Copilot CLI](https://docs.github.com/copilot/how-tos/use-copilot-agents/use-copilot-cli). |
| **Gemini CLI** | Start in **Plan Mode**: `gemini --approval-mode=plan`, or `/plan` / `Shift+Tab` in session. Plan Mode blocks shell commands (read-only research only). Approve into **Default** (manual-approval) mode — never `Auto-Edit` or YOLO. See [Plan Mode](https://geminicli.com/docs/cli/plan-mode/). |

## Two-phase execution model

Both prompts below run in two explicit phases. This matters because some tools (notably
Gemini Plan Mode) **block shell commands while planning**, so preflight cannot run there:

1. **Phase 1 — planning posture (no shell).** The agent reads the canonical files and
   presents the intended non-mutating checks plus the full list of intended mutations and
   approval gates. No commands run.
2. **Phase 2 — default / manual-approval mode.** You switch the tool out of planning into
   its manual-approval execution mode. The agent runs the non-mutating preflight, reports
   results, then **waits for a separate human approval** before any cluster mutation.

After you approve, keep the tool in manual/default approval — **not** Claude `auto`, Codex
`Agent (Full Access)`, Copilot autopilot/"approve for the rest of the session", or Gemini
`Auto-Edit`. See [Safety rules](#safety-rules).

## Prompt: local demo

Paste this once you are in the planning posture (Phase 1):

```
You are helping me evaluate Koshi's local kind demo under my supervision. Work in two
phases and never skip an approval gate.

PHASE 1 (planning posture — do not run any shell commands yet):
- Read demo/local/README.md, demo/local/setup.sh, and demo/local/teardown.sh. Do NOT edit
  them or any other file.
- Present: (a) the non-mutating preflight checks you will run, (b) the complete list of
  cluster mutations setup.sh performs, (c) the exact target kube context (kind-koshi-demo),
  and (d) the cleanup you will run at the end.
- Then stop and wait for me to switch you into manual-approval execution mode.

PHASE 2 (manual-approval mode):
- Run ONLY non-mutating checks and report results: `git status`; that docker/kind/helm/
  kubectl/jq/curl are present; Docker daemon is running; and the current kube context.
- Report any pre-existing working-tree changes before going further.
- STOP and wait for my explicit approval before running ./demo/local/setup.sh.
- After I approve, run ./demo/local/setup.sh (run it from the repo root, or cd demo/local
  first) and report: whether the sidecar was injected, the new-event delta, the metric
  (koshi_listener_decisions_total) delta, that the original kube context was restored on
  exit, and anything left running.
- STOP and wait for a SEPARATE approval before teardown. Cleanup is
  ./demo/local/teardown.sh from the repo root, or ./teardown.sh from the demo/local
  directory; after I approve, run the one matching your working directory and confirm the
  kind cluster was deleted.

Rules: treat the canonical scripts as the source of truth — run them as-is, do not rewrite
them. If scoped context checks, sidecar injection, pod pinning, or the telemetry
delta assertions fail, STOP and report; do not improvise workarounds. Never switch kube
contexts, weaken the webhook failurePolicy, or broaden namespace restarts.
```

## Prompt: real-cluster onboarding

Paste this once you are in the planning posture (Phase 1). This targets a **real** cluster,
so the approval gates and credential handling are stricter.

```
You are helping me onboard Koshi onto a real cluster under my supervision, following
docs/onboarding.md. Work in two phases and never skip an approval gate.

PHASE 1 (planning posture — do not run any shell commands yet):
- Read docs/onboarding.md. Do NOT rewrite, reorder, or bypass its commands; you will run
  them as written.
- Present the intended non-mutating checks, the full list of mutations, and every approval
  gate (Helm install, canary creation, optional OpenAI Secret, real-workload adoption, and
  cleanup — noting cleanup differs for canary-only vs after-adoption).
- Then stop and wait for me to switch you into manual-approval execution mode.

PHASE 2 (manual-approval mode):
- Run only non-mutating checks and report: the active kube context, the cluster API
  endpoint, the Helm release name, and the disposable canary namespace you propose
  (default: koshi-eval). Report any pre-existing working-tree changes.
- STOP and wait for approval before the Helm install and before creating the canary.
- Run ONLY the canary path first (install + wait for injector + canary verification).
  Report the exact event evidence (event_type=listener_shadow, namespace, workload_name,
  provider, estimated_tokens, decision_shadow), the metric delta, AND the final result line
  the block prints — "Canary verification PASSED" or "Canary verification FAILED" (the block
  returns non-zero on failure). If the canary returns non-zero / prints FAILED, STOP and
  report; do NOT proceed to the OpenAI step, to adoption, or to treating cleanup as success.
- Optional OpenAI Secret step (only if I ask for the real-upstream proof): do NOT run it
  yourself. PAUSE and instruct me to run the documented hidden-key Secret block — both the
  `read -rs` prompt AND the `printf ... | kubectl create secret ... --from-file=.../dev/stdin`
  command — directly in my own terminal. You must never run, capture, echo, store, or relay
  that block or my key through your tool interface.
- Before adopting any real workload: require a SEPARATE approval and confirm the adoption
  inputs with me — NS, APP, and POD_SELECTOR. Validate that the pinned pod's controller
  owner reference resolves to APP; STOP on any ambiguity or ownership mismatch. The canonical
  onboarding assumes Deployment adoption — a non-Deployment controller (StatefulSet,
  DaemonSet, Job, or standalone pod) requires adapting the documented flow and a separate
  approval.
- STOP and wait for a SEPARATE approval before cleanup. Cleanup is NOT just deleting the
  canary namespace — in BOTH cases the Helm release and its leftover cluster-scoped
  resources must be removed and verified:
    * Canary only (no adoption): delete the canary namespace, then run the documented Helm
      uninstall + cert-gen RBAC and hook-created TLS Secret cleanup + exact verification.
    * After real-workload adoption: follow docs/onboarding.md "Stop evaluating Koshi" —
      remove the namespace label; inventory residual koshi-listener pods and report their
      resolved controller owners; get my per-workload approval before any restart/delete/
      recreate; then the same Helm uninstall + cert-gen RBAC + TLS Secret cleanup; delete
      the namespace only when safe (created by this evaluation); and run the exact
      name-based verification.

Rules: STOP and report — do not improvise — on an unexpected kube context, missing sidecar
injection, failed telemetry deltas, a FAILED/non-zero canary result, or any ambiguity or
ownership mismatch about the target workload. Never weaken the webhook failurePolicy,
broaden to namespace-wide restarts, switch clusters, or alter the canonical docs/scripts
without a separate, explicit change request from me.
```

## Safety rules

- **Local resources only.** Use a machine where Docker and kubeconfig actually exist. Cloud
  agents are unsuitable unless their environment explicitly provides those resources.
- **Keep sandboxing and permission prompts enabled** for the whole session.
- **After plan approval, execute only in manual / default approval modes.** Explicitly no
  Claude `auto` mode, no Copilot autopilot, no Gemini `Auto-Edit`, no Codex
  `Agent (Full Access)` for these cluster operations. Each mutating command — or an
  explicitly described bundled script invocation such as `setup.sh` — requires approval;
  never grant blanket session authorization (e.g. Copilot's "approve for the rest of the
  session").
- **Never use bypass modes:** Codex full-access / bypass, Claude `bypassPermissions`
  (`--dangerously-skip-permissions`), Copilot `--allow-all` / `--yolo`, or Gemini YOLO.
- **Credentials never touch the agent.** Provider API keys are entered only by you, via the
  documented hidden terminal prompt. The agent must not request, echo, store, or paste them.
- **No scope drift.** Agents may explain failures, but must not weaken `failurePolicy`,
  broaden namespace restarts, switch clusters, or modify the canonical scripts/docs without
  a separate change request.
- **Preserve existing work.** Report any pre-existing working-tree changes before execution
  and leave them intact.

## See also

- [Local demo walkthrough](../demo/local/README.md) — canonical kind-cluster demo
- [Koshi onboarding](onboarding.md) — canonical real-cluster onboarding
- [Kubernetes observability guide](kubernetes-observability.md) — events, metrics, queries
