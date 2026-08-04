{{/*
All metadata this chart attaches lives in this file. Resource templates call
"pa.labels" and "pa.annotations" and add nothing of their own, so the recommended
set is defined once and cannot drift between templates.

Call shape, for every resource:

  metadata:
    name: ...
    labels:
      {{- include "pa.labels" (dict "ctx" . "component" "object-store") | nindent 4 }}
    {{- with include "pa.annotations" (dict "ctx" . "description" "…") }}
    annotations:
      {{- . | nindent 4 }}
    {{- end }}

`component` names the role the object plays. Everything else is derived from
the chart and release, so no template passes a name or a version.
*/}}

{{/*
Chart name and version, as a label value.

A SemVer build identifier uses "+", which is not valid in a label value, so it
is replaced. Truncated to the 63-character limit Kubernetes enforces on label
values, and any trailing "-" left by that truncation is removed, since a label
value may not end in a separator.
*/}}
{{- define "pa.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels: every label in Helm's Standard Labels table
(helm.sh/docs/chart_best_practices/labels/) — the four REC and all three OPT.

Takes a dict: `ctx` (root context) and optional `component`.

name and version follow that table rather than the Kubernetes common-labels
doc, and the two genuinely disagree. Kubernetes' example labels MySQL-under-
WordPress `name: mysql, version: 5.7.21`, naming each application; Helm says
name "should be the app name, reflecting the entire app" and version "can be
set to {{ .Chart.AppVersion }}". Helm's reading assumes one app per chart and
leaves component to distinguish the pieces, which is what we do: every object
is name: pocket-advisor, and component says object-store / database /
message-bus / rustfs-setup.

An earlier revision used the Kubernetes reading, with name per workload and
version from the image tag. Reverted deliberately — the image tag is still on
the pod spec, so nothing is lost that was not already visible, and having one
answer to "what is app.kubernetes.io/name here" is worth more than a second
copy of the image version.

app.kubernetes.io/part-of is load-bearing rather than decorative: the
VMServiceScrape selector matches on it, so a workload missing this label is
silently not scraped (ingestion-design.md §9.2). `make destroy-infra` likewise
selects the setup Jobs on app.kubernetes.io/component.

None of this is a deletion index. Helm does not consult labels to decide what
to remove on uninstall — that is driven entirely by the release manifest stored
in sh.helm.release.v1.<name>.v<N>.

Deliberately NOT wired into any workload's .spec.selector.matchLabels, which
still match the legacy `app: <name>` key. .spec.selector is immutable on a live
StatefulSet, so changing it fails `helm upgrade` outright with "updates to
statefulset spec ... are forbidden", and the only way out is recreating the
StatefulSet — which here means detaching the PVCs holding Tier 1 and the
JetStream state. Not a trade worth making for a cosmetic key change.
*/}}
{{- define "pa.labels" -}}
{{- $ctx := .ctx -}}
helm.sh/chart: {{ include "pa.chart" $ctx }}
app.kubernetes.io/name: {{ $ctx.Chart.Name }}
app.kubernetes.io/instance: {{ $ctx.Release.Name }}
app.kubernetes.io/version: {{ $ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ $ctx.Release.Service }}
app.kubernetes.io/part-of: rag-ingestion-engine
{{- with .component }}
app.kubernetes.io/component: {{ . }}
{{- end }}
{{- end }}

{{/*
Common annotations.

Takes a dict: `ctx` (root context) and optional `description`.

Deliberately does not emit meta.helm.sh/release-name or -namespace. Those look
like they belong here, but Helm writes them itself on install and upgrade, and
uses them — together with app.kubernetes.io/managed-by — for its ownership
check, refusing to adopt a resource another release already owns. They are
Helm's to manage; a chart that also declares them is duplicating machine-owned
metadata for no gain. Confirmed by rendering: `helm template` emits no
annotations at all, while the same objects in a live release carry both.

Note this is distinct from the field-manager conflicts a `helm upgrade` can hit
on .data.nats-server.conf. Those come from Kubernetes server-side apply
tracking which manager owns which field, not from these annotations (README §9).

Renders empty when no annotation applies, so call sites must guard with `with`
rather than emitting a bare `annotations:` key.
*/}}
{{- define "pa.annotations" -}}
{{- with .description }}
kubernetes.io/description: {{ . | quote }}
{{- end }}
{{- end }}
