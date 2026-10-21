{{/* Names: <release>-kista unless the release name holds "kista", at most 63 characters. */}}
{{- define "kista.fullname" -}}
{{- if contains "kista" .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-kista" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* A name with a suffix, the suffix kept whole; at most 63 characters, or the third argument. */}}
{{- define "kista.name" -}}
{{- $root := index . 0 -}}{{- $suffix := index . 1 -}}
{{- $max := 63 -}}{{- if gt (len .) 2 -}}{{- $max = index . 2 -}}{{- end -}}
{{- $base := include "kista.fullname" $root | trunc (int (sub (sub $max 1) (len $suffix))) | trimSuffix "-" -}}
{{- printf "%s-%s" $base $suffix -}}
{{- end -}}

{{- define "kista.selectorLabels" -}}
app.kubernetes.io/name: kista
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end -}}

{{- define "kista.labels" -}}
{{ include "kista.selectorLabels" . }}
app.kubernetes.io/version: {{ include "kista.version" . | trunc 63 | trimSuffix "-" | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name (.Chart.Version | replace "+" "_") | trunc 63 | trimSuffix "-" | quote }}
{{- end -}}

{{- define "kista.version" -}}
{{- .Values.image.tag | default .Chart.AppVersion -}}
{{- end -}}

{{- define "kista.image" -}}
{{- if .Values.image.digest -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" .Values.image.digest) -}}
{{- fail "image.digest: sha256:<64 hex digits>" -}}
{{- end -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (include "kista.version" .) -}}
{{- end -}}
{{- end -}}

{{- define "kista.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- .Values.serviceAccount.name | default (include "kista.fullname" .) -}}
{{- else -}}
{{- .Values.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{/* The config values, each string with "{{" rendered by tpl with configTemplate (spec 0025):
     (dict "root" $ "out" $out) sets $out.v. Off: the values as they are. Rendered once per helm
     run and kept on the root context, so the ConfigMap, its checksum and the checks see one
     result. */}}
{{- define "kista.userConfig" -}}
{{- $c := .root.Values.config | default dict -}}
{{- if .root.Values.configTemplate -}}
{{- if not (hasKey .root "kistaRenderedConfig") -}}
{{- $acc := dict -}}
{{- include "kista.tplNode" (dict "node" $c "root" .root "out" $acc "path" "config") -}}
{{- $_ := set .root "kistaRenderedConfig" $acc.v -}}
{{- end -}}
{{- $c = get .root "kistaRenderedConfig" -}}
{{- end -}}
{{- $_ := set .out "v" $c -}}
{{- end -}}

{{/* A copy of a tree with tpl applied to each string that holds "{{": (dict "node" "root" "out"
     "path"). A template renders within its string, so it cannot add keys; it may read values
     only (no lookup, tpl, include, template or getHostByName), and must not render empty. */}}
{{- define "kista.tplNode" -}}
{{- $root := .root -}}{{- $path := .path -}}
{{- if kindIs "map" .node -}}
{{- $m := dict -}}
{{- range $k, $v := .node -}}
{{- $acc := dict -}}
{{- include "kista.tplNode" (dict "node" $v "root" $root "out" $acc "path" (printf "%s.%s" $path $k)) -}}
{{- $_ := set $m $k $acc.v -}}
{{- end -}}
{{- $_ := set .out "v" $m -}}
{{- else if kindIs "slice" .node -}}
{{- $l := list -}}
{{- range $i, $v := .node -}}
{{- $acc := dict -}}
{{- include "kista.tplNode" (dict "node" $v "root" $root "out" $acc "path" (printf "%s[%d]" $path $i)) -}}
{{- $l = append $l $acc.v -}}
{{- end -}}
{{- $_ := set .out "v" $l -}}
{{- else if and (kindIs "string" .node) (contains "{{" .node) -}}
{{- range regexFindAll "(?s)\\{\\{.*?\\}\\}" .node -1 -}}
{{- with regexFind "\\b(lookup|tpl|include|template|getHostByName)\\b" . -}}
{{- fail (printf "configTemplate: %s calls %s; a config template may read values only" $path .) -}}
{{- end -}}
{{- end -}}
{{- $r := tpl .node $root -}}
{{- if not (trim $r) -}}
{{- fail (printf "configTemplate: %s renders empty (a missing value?); write {{ required \"…\" .Values.… }} to name it" $path) -}}
{{- end -}}
{{- $_ := set .out "v" $r -}}
{{- else -}}
{{- $_ := set .out "v" .node -}}
{{- end -}}
{{- end -}}

{{- define "kista.sqlite" -}}
{{- $out := dict -}}{{- include "kista.userConfig" (dict "root" . "out" $out) -}}
{{- if eq (dig "store" "kind" "" $out.v) "sqlite" -}}true{{- end -}}
{{- end -}}

{{/* One pod at a time: SQLite, or fs blobs on a volume that is not ReadWriteMany. */}}
{{- define "kista.singleVolume" -}}
{{- if or (include "kista.sqlite" .) (and .Values.blobVolume.enabled (ne .Values.blobVolume.accessMode "ReadWriteMany")) -}}true{{- end -}}
{{- end -}}

{{/* The replicas: one with SQLite. */}}
{{- define "kista.replicas" -}}
{{- if include "kista.sqlite" . -}}1{{- else -}}{{ int .Values.replicas }}{{- end -}}
{{- end -}}

{{- define "kista.networkPolicy" -}}
{{- $np := .Values.networkPolicy.enabled -}}
{{- if kindIs "invalid" $np -}}
{{- if eq .Values.mode "behind_proxy" -}}true{{- end -}}
{{- else if $np -}}true{{- end -}}
{{- end -}}

{{/* UDP and TCP ports as NetworkPolicy ports (DNS). */}}
{{- define "kista.dnsPorts" -}}
{{- $out := list -}}
{{- range . -}}{{- $out = concat $out (list (dict "port" (int .) "protocol" "UDP") (dict "port" (int .) "protocol" "TCP")) -}}{{- end -}}
{{- toYaml $out -}}
{{- end -}}

{{/* TCP ports as NetworkPolicy ports. */}}
{{- define "kista.tcpPorts" -}}
{{- $out := list -}}
{{- range . -}}{{- $out = append $out (dict "port" (int .) "protocol" "TCP") -}}{{- end -}}
{{- toYaml $out -}}
{{- end -}}

{{/* The cloud metadata endpoints by name (networkPolicy.egress.cloudMetadata): (list cidr port). */}}
{{- define "kista.metadataEndpoints" -}}
gke:
  # the GKE metadata server (Workload Identity): Dataplane V2, and 1.21 and later
  - [169.254.169.254/32, 80]
  - [169.254.169.252/32, 988]
aws-pod-identity:
  # the EKS Pod Identity agent
  - [169.254.170.23/32, 80]
  - ["fd00:ec2::23/128", 80]
imds:
  # the instance metadata service: EC2 (identity kind imds), Azure (kind managed)
  - [169.254.169.254/32, 80]
{{- end -}}

{{/* The ranges kista never reaches, even when the config allows them (internal/egress, never). */}}
{{- define "kista.neverRanges" -}}
[169.254.0.0/16, 168.63.129.16/32, 100.100.100.200/32, "fe80::/10", "fd00:ec2::/112"]
{{- end -}}

{{/* A CIDR's address as bits: 32 or 128 characters; empty when the chart cannot read it. */}}
{{- define "kista.cidrBits" -}}
{{- $addr := index (splitList "/" .) 0 -}}
{{- if and (contains ":" $addr) (not (contains "." $addr)) -}}
{{- $halves := splitList "::" (lower $addr) -}}
{{- $left := list -}}{{- with index $halves 0 -}}{{- $left = splitList ":" . -}}{{- end -}}
{{- $right := list -}}{{- if gt (len $halves) 1 -}}{{- with index $halves 1 -}}{{- $right = splitList ":" . -}}{{- end -}}{{- end -}}
{{- $groups := $left -}}
{{- if gt (len $halves) 1 -}}
{{- range until (int (sub 8 (add (len $left) (len $right)))) -}}{{- $groups = append $groups "0" -}}{{- end -}}
{{- end -}}
{{- $groups = concat $groups $right -}}
{{- $hex := dict "0" "0000" "1" "0001" "2" "0010" "3" "0011" "4" "0100" "5" "0101" "6" "0110" "7" "0111" "8" "1000" "9" "1001" "a" "1010" "b" "1011" "c" "1100" "d" "1101" "e" "1110" "f" "1111" -}}
{{- $bits := "" -}}
{{- range $groups -}}
{{- $g := printf "%s%s" (repeat (int (sub 4 (len .))) "0") . -}}
{{- range $c := splitList "" $g -}}{{- $bits = printf "%s%s" $bits (get $hex $c) -}}{{- end -}}
{{- end -}}
{{- if eq (len $bits) 128 -}}{{- $bits -}}{{- end -}}
{{- else if regexMatch "^[0-9]{1,3}(\\.[0-9]{1,3}){3}$" $addr -}}
{{- range splitList "." $addr -}}{{- printf "%08b" (atoi .) -}}{{- end -}}
{{- end -}}
{{- end -}}

{{/* Whether the first CIDR contains the second (same family, as far as the chart can read them):
     (list outer inner). */}}
{{- define "kista.cidrContains" -}}
{{- $o := index . 0 -}}{{- $i := index . 1 -}}
{{- $ob := include "kista.cidrBits" $o -}}{{- $ib := include "kista.cidrBits" $i -}}
{{- $on := atoi (last (splitList "/" $o)) -}}{{- $in := atoi (last (splitList "/" $i)) -}}
{{- if and $ob (eq (len $ob) (len $ib)) (le $on $in) (le $on (len $ob)) -}}
{{- if eq (trunc $on $ob) (trunc $on $ib) -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{/* An ipBlock rule to a CIDR on TCP ports, less the ranges kista never reaches; none when the
     CIDR lies inside one of them: (list cidr ports). */}}
{{- define "kista.allowRule" -}}
{{- $cidr := index . 0 -}}
{{- $inside := false -}}{{- $except := list -}}
{{- range $n := include "kista.neverRanges" . | fromYamlArray -}}
{{- if include "kista.cidrContains" (list $n $cidr) -}}{{- $inside = true -}}
{{- else if include "kista.cidrContains" (list $cidr $n) -}}{{- $except = append $except $n -}}
{{- end -}}
{{- end -}}
{{- if not $inside -}}
{{- $block := dict "cidr" $cidr -}}
{{- if $except -}}{{- $_ := set $block "except" $except -}}{{- end -}}
{{- toYaml (list (dict "to" (list (dict "ipBlock" $block)) "ports" (include "kista.tcpPorts" (index . 1) | fromYamlArray))) -}}
{{- else -}}
[]
{{- end -}}
{{- end -}}

{{/* config.egress.proxy, rendered, as JSON: [host, port, whether host is an IP address]. */}}
{{- define "kista.proxy" -}}
{{- $out := dict -}}{{- include "kista.userConfig" (dict "root" . "out" $out) -}}
{{- $proxy := toString (dig "egress" "proxy" "" $out.v) -}}
{{- if $proxy -}}
{{- $u := urlParse $proxy -}}
{{- $host := $u.hostname -}}
{{- $port := regexFind "[0-9]+$" $u.host -}}
{{- $ip := or (regexMatch "^[0-9]{1,3}(\\.[0-9]{1,3}){3}$" $host) (contains ":" $host) -}}
{{- toJson (list $host $port $ip) -}}
{{- end -}}
{{- end -}}

{{/* The egress NetworkPolicy's rules (spec 0025), as a YAML list. */}}
{{- define "kista.egressRules" -}}
{{- $eg := .Values.networkPolicy.egress -}}
{{- $rules := list -}}
{{- /* DNS: UDP and TCP to the cluster's DNS pods, and to DNS that is no pod */ -}}
{{- if $eg.dns.enabled -}}
{{- $peer := dict "podSelector" $eg.dns.podSelector -}}
{{- with $eg.dns.namespaceSelector -}}{{- $_ := set $peer "namespaceSelector" . -}}{{- end -}}
{{- if not (or $eg.dns.podSelector $eg.dns.namespaceSelector) -}}
{{- /* the default: kube-dns or CoreDNS, as EKS, AKS, GKE, k3s, kind and Gardener label them */ -}}
{{- $peer = dict "namespaceSelector" (dict "matchLabels" (dict "kubernetes.io/metadata.name" "kube-system")) "podSelector" (dict "matchLabels" (dict "k8s-app" "kube-dns")) -}}
{{- end -}}
{{- $rules = append $rules (dict "to" (list $peer) "ports" (include "kista.dnsPorts" $eg.dns.ports | fromYamlArray)) -}}
{{- range $eg.dns.ipBlocks -}}
{{- $rules = append $rules (dict "to" (list (dict "ipBlock" (dict "cidr" .cidr))) "ports" (include "kista.dnsPorts" (.ports | default $eg.dns.ports) | fromYamlArray)) -}}
{{- end -}}
{{- end -}}
{{- /* public https: every address but the private and special ranges, as kista's egress */ -}}
{{- if $eg.public.enabled -}}
{{- $to := list -}}
{{- range $cidr := $eg.public.cidrs -}}
{{- $block := dict "cidr" $cidr -}}
{{- if hasSuffix "/0" $cidr -}}
{{- $v6 := contains ":" $cidr -}}
{{- $except := list -}}
{{- range $eg.public.except -}}{{- if eq (contains ":" .) $v6 -}}{{- $except = append $except . -}}{{- end -}}{{- end -}}
{{- if $except -}}{{- $_ := set $block "except" $except -}}{{- end -}}
{{- end -}}
{{- $to = append $to (dict "ipBlock" $block) -}}
{{- end -}}
{{- $rules = append $rules (dict "to" $to "ports" (include "kista.tcpPorts" $eg.public.ports | fromYamlArray)) -}}
{{- end -}}
{{- /* the private networks kista's configuration allows: egress.allow, auth.server_egress_allow,
       upstreams.credentials[].allow, events.sinks[].allow; less what kista never reaches */ -}}
{{- $acc := dict "maps" list -}}
{{- include "kista.walk" (dict "node" (include "kista.config" . | fromYaml) "acc" $acc) -}}
{{- range $acc.maps -}}
{{- if hasKey . "cidr" -}}
{{- $rules = concat $rules (include "kista.allowRule" (list (toString .cidr) (.ports | default (list 443))) | fromYamlArray) -}}
{{- end -}}
{{- end -}}
{{- /* egress.proxy at an IP address; a host name needs extraRules (kista.validate) */ -}}
{{- with include "kista.proxy" . -}}
{{- $p := fromJsonArray . -}}
{{- if index $p 2 -}}
{{- $cidr := printf "%s/32" (index $p 0) -}}{{- if contains ":" (index $p 0) -}}{{- $cidr = printf "%s/128" (index $p 0) -}}{{- end -}}
{{- $rules = concat $rules (include "kista.allowRule" (list $cidr (list (index $p 1))) | fromYamlArray) -}}
{{- end -}}
{{- end -}}
{{- /* the cloud metadata endpoints the identity uses */ -}}
{{- $endpoints := include "kista.metadataEndpoints" . | fromYaml -}}
{{- range $eg.cloudMetadata -}}
{{- range get $endpoints . -}}
{{- $rules = append $rules (dict "to" (list (dict "ipBlock" (dict "cidr" (index . 0)))) "ports" (list (dict "port" (int (index . 1)) "protocol" "TCP"))) -}}
{{- end -}}
{{- end -}}
{{- /* the operator's own: the database, a key source, a collector, a token service */ -}}
{{- $rules = concat $rules ($eg.extraRules | default list) -}}
{{- toYaml $rules -}}
{{- end -}}

{{/* Whether a path is dir or under it. */}}
{{- define "kista.under" -}}
{{- $p := index . 0 -}}{{- $dir := index . 1 -}}
{{- if or (eq $p $dir) (hasPrefix (printf "%s/" $dir) $p) -}}true{{- end -}}
{{- end -}}

{{/* A Go duration ("30s", "1m30s", "500ms") in whole seconds, rounded up. */}}
{{- define "kista.seconds" -}}
{{- $d := toString . -}}
{{- if not (regexMatch "^([0-9]+(\\.[0-9]+)?(h|m|s|ms|us|ns))+$" $d) -}}
{{- fail (printf "the chart reads durations like 30s or 1m30s, not %q: set terminationGracePeriodSeconds" $d) -}}
{{- end -}}
{{- $total := 0.0 -}}
{{- range regexFindAll "[0-9]+(\\.[0-9]+)?(h|ms|us|ns|m|s)" $d -1 -}}
{{- $n := float64 (regexFind "^[0-9]+(\\.[0-9]+)?" .) -}}
{{- $u := regexReplaceAll "^[0-9]+(\\.[0-9]+)?" . "" -}}
{{- $mul := dict "h" 3600.0 "m" 60.0 "s" 1.0 "ms" 0.001 "us" 0.000001 "ns" 0.000000001 -}}
{{- $total = addf $total (mulf $n (get $mul $u)) -}}
{{- end -}}
{{- ceil $total | int -}}
{{- end -}}

{{/* A kista size (bytes, or a number with KiB, MiB, GiB) in MiB, rounded up. */}}
{{- define "kista.mib" -}}
{{- $s := toString . -}}
{{- $m := regexFind "^[0-9]+" $s -}}
{{- if not (regexMatch "^[0-9]+ *(KiB|MiB|GiB)?$" $s) -}}
{{- fail (printf "blob.max_body: a size like 1GiB, not %q (or set spool.sizeLimit)" $s) -}}
{{- end -}}
{{- $n := float64 $m -}}
{{- if hasSuffix "GiB" $s -}}{{- $n = mulf $n 1024.0 -}}
{{- else if hasSuffix "KiB" $s -}}{{- $n = divf $n 1024.0 -}}
{{- else if not (hasSuffix "MiB" $s) -}}{{- $n = divf $n 1048576.0 -}}
{{- end -}}
{{- ceil $n | int -}}
{{- end -}}

{{/* Every map in a tree, into .acc.maps. */}}
{{- define "kista.walk" -}}
{{- $acc := .acc -}}
{{- if kindIs "map" .node -}}
{{- $_ := set $acc "maps" (append $acc.maps .node) -}}
{{- range $k, $v := .node }}{{ include "kista.walk" (dict "node" $v "acc" $acc) }}{{ end -}}
{{- else if kindIs "slice" .node -}}
{{- range .node }}{{ include "kista.walk" (dict "node" . "acc" $acc) }}{{ end -}}
{{- end -}}
{{- end -}}

{{/* kista.yaml as the pods read it: config with the chart's keys (values.yaml says which). */}}
{{- define "kista.config" -}}
{{- $out := dict -}}{{- include "kista.userConfig" (dict "root" . "out" $out) -}}
{{- $c := deepCopy $out.v -}}
{{- $serve := get $c "serve" | default dict -}}
{{- $_ := set $c "serve" $serve -}}
{{- $listeners := list -}}
{{- if get $serve "listeners" -}}
{{- $listeners = get $serve "listeners" -}}
{{- else if eq .Values.mode "tls" -}}
{{- $listeners = list (dict "addr" ":8443" "scheme" "dual" "tls" (dict "cert_file" "/etc/kista/tls/tls.crt" "key_file" "/etc/kista/tls/tls.key")) -}}
{{- else -}}
{{- $listeners = list (dict "addr" ":8443" "scheme" "https" "behind_proxy" true) -}}
{{- end -}}
{{- $_ = set $serve "listeners" (append $listeners (dict "addr" ":8081" "scheme" "health")) -}}
{{- $blob := get $c "blob" | default dict -}}
{{- $_ = set $c "blob" $blob -}}
{{- $_ = set $blob "spool_dir" "/var/lib/kista/tmp/spool" -}}
{{- if include "kista.sqlite" . -}}
{{- $store := get $c "store" -}}
{{- $sqlite := get $store "sqlite" | default dict -}}
{{- $_ = set $store "sqlite" $sqlite -}}
{{- if not (get $sqlite "path") -}}
{{- $_ = set $sqlite "path" "/var/lib/kista/data/kista.db" -}}
{{- end -}}
{{- if not (get $blob "domains") -}}
{{- $_ = set $blob "domains" (list (dict "name" "default" "kind" "fs" "fs" (dict "root" "/var/lib/kista/data/blobs"))) -}}
{{- end -}}
{{- end -}}
{{- toYaml $c -}}
{{- end -}}

{{/* The checks helm install fails on (spec 0013). */}}
{{- define "kista.validate" -}}
{{- $v := .Values -}}
{{- $out := dict -}}{{- include "kista.userConfig" (dict "root" . "out" $out) -}}
{{- $user := $out.v -}}
{{- $c := include "kista.config" . | fromYaml -}}
{{- if not (dig "store" "kind" "" $user) -}}
{{- fail "config.store.kind is required" -}}
{{- end -}}
{{- if not (has $v.mode (list "behind_proxy" "tls")) -}}
{{- fail "mode: behind_proxy or tls" -}}
{{- end -}}
{{- if and (eq $v.mode "tls") (not $v.tlsSecret) -}}
{{- fail "mode tls needs tlsSecret, a kubernetes.io/tls Secret" -}}
{{- end -}}
{{- /* listeners */ -}}
{{- $userListeners := dig "serve" "listeners" list $user -}}
{{- $serving := false -}}
{{- range $userListeners -}}
{{- if eq (toString .scheme) "health" -}}
{{- fail "config.serve.listeners: the chart adds the health listener on :8081; do not define one" -}}
{{- end -}}
{{- if hasSuffix ":8081" (toString .addr) -}}
{{- fail "config.serve.listeners: :8081 is the chart's health port" -}}
{{- end -}}
{{- if hasSuffix ":8443" (toString .addr) -}}{{- $serving = true -}}{{- end -}}
{{- end -}}
{{- if and $userListeners (not $serving) -}}
{{- fail "config.serve.listeners: the Service sends to :8443; one listener must use it" -}}
{{- end -}}
{{- range $c.serve.listeners -}}
{{- if and .behind_proxy (not (dig "serve" "trusted_proxies" list $c)) -}}
{{- fail "a behind_proxy listener needs config.serve.trusted_proxies: the ingress controller's or load balancer's addresses (in practice the pod CIDR)" -}}
{{- end -}}
{{- end -}}
{{- /* storage */ -}}
{{- $sqlite := include "kista.sqlite" . -}}
{{- if $sqlite -}}
{{- $p := toString $c.store.sqlite.path -}}
{{- if or (ne (clean $p) $p) (not (include "kista.under" (list $p "/var/lib/kista/data"))) -}}
{{- fail "config.store.sqlite.path must be a clean path under /var/lib/kista/data, the persistent volume" -}}
{{- end -}}
{{- end -}}
{{- if and $v.adminJobs (include "kista.singleVolume" .) -}}
{{- fail "adminJobs: refused with SQLite or a blobVolume that is not ReadWriteMany (a second pod cannot mount the volume); run kubectl exec" -}}
{{- end -}}
{{- if and (eq $v.mode "behind_proxy") $v.service.type (ne $v.service.type "ClusterIP") -}}
{{- fail "service.type: mode behind_proxy serves plain http for an ingress in the cluster; a LoadBalancer or NodePort would expose it (use mode tls)" -}}
{{- end -}}
{{- if and (eq (toString (dig "profile" "" $user)) "dev") (not $v.devProfile) -}}
{{- fail "config.profile dev relaxes key and identity rules; set devProfile: true to deploy it anyway (never in production)" -}}
{{- end -}}
{{- /* the pod */ -}}
{{- if lt (int $v.preStopSeconds) 1 -}}
{{- fail "preStopSeconds: at least 1" -}}
{{- end -}}
{{- if le (int (include "kista.grace" .)) (int $v.preStopSeconds) -}}
{{- fail "terminationGracePeriodSeconds must exceed preStopSeconds" -}}
{{- end -}}
{{- range $k, $_ := $v.podLabels -}}
{{- if has $k (list "app.kubernetes.io/name" "app.kubernetes.io/instance" "app.kubernetes.io/component") -}}
{{- fail (printf "podLabels: %s is the chart's selector" $k) -}}
{{- end -}}
{{- end -}}
{{- if and $v.ingress.enabled (not $v.ingress.hosts) -}}
{{- fail "ingress.hosts: at least one entry (an entry without host serves every host)" -}}
{{- end -}}
{{- $names := dict -}}
{{- range $v.secretVolumes -}}
{{- $m := clean (toString .mountPath) -}}
{{- if or (eq $m "/etc/kista") (include "kista.under" (list $m "/etc/kista/tls")) (include "kista.under" (list $m "/etc/kista/kista.yaml")) (include "kista.under" (list "/var/lib/kista" $m)) (include "kista.under" (list $m "/var/lib/kista")) -}}
{{- fail (printf "secretVolumes %s: mountPath %s overlaps the chart's paths (/etc/kista, its config and tls, /var/lib/kista)" .name .mountPath) -}}
{{- end -}}
{{- if hasKey $names (printf "s-%s" .name) -}}{{- fail (printf "secretVolumes: %s twice" .name) -}}{{- end -}}
{{- $_ := set $names (printf "s-%s" .name) true -}}
{{- end -}}
{{- range $v.projectedTokens -}}
{{- if hasKey $names (printf "t-%s" .name) -}}{{- fail (printf "projectedTokens: %s twice" .name) -}}{{- end -}}
{{- $_ := set $names (printf "t-%s" .name) true -}}
{{- end -}}
{{- range $v.adminJobs -}}
{{- if hasKey $names (printf "j-%s" .name) -}}{{- fail (printf "adminJobs: %s twice" .name) -}}{{- end -}}
{{- $_ := set $names (printf "j-%s" .name) true -}}
{{- end -}}
{{- $replicas := int (include "kista.replicas" .) -}}
{{- $domains := dig "blob" "domains" list $c -}}
{{- if not $domains -}}
{{- $domains = list (dict "name" "default" "kind" "fs" "fs" (dict "root" "/var/lib/kista/blobs")) -}}
{{- end -}}
{{- range $domains -}}
{{- if eq (toString .kind) "fs" -}}
{{- $root := dig "fs" "root" "" . -}}
{{- $onData := and $sqlite (include "kista.under" (list $root "/var/lib/kista/data")) -}}
{{- $onBlob := and $v.blobVolume.enabled (include "kista.under" (list $root "/var/lib/kista/blobs")) -}}
{{- if not (or $onData $onBlob) -}}
{{- fail (printf "blob domain %s: fs root %s is on no volume: use blobVolume (/var/lib/kista/blobs) or a remote store" .name $root) -}}
{{- end -}}
{{- if and (gt $replicas 1) (not (and $onBlob (eq $v.blobVolume.accessMode "ReadWriteMany"))) -}}
{{- fail (printf "blob domain %s: %d replicas need a remote store (s3, azureblob, gcs) or fs on a ReadWriteMany blobVolume" .name $replicas) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* tokens */ -}}
{{- range $v.projectedTokens -}}
{{- if or (not .name) (not .mountPath) (not .audience) -}}
{{- fail "projectedTokens: each needs name, mountPath and audience" -}}
{{- end -}}
{{- if has .audience (list "kubernetes.default.svc" "kubernetes.default.svc.cluster.local" "https://kubernetes.default.svc" "https://kubernetes.default.svc.cluster.local") -}}
{{- fail (printf "projectedTokens %s: the audience must not be the API server's" .name) -}}
{{- end -}}
{{- $exp := int (.expirationSeconds | default 3600) -}}
{{- if or (lt $exp 600) (gt $exp 3600) -}}
{{- fail (printf "projectedTokens %s: expirationSeconds 600..3600" .name) -}}
{{- end -}}
{{- end -}}
{{- /* identities: the values and the config name the same one */ -}}
{{- $acc := dict "maps" list -}}
{{- include "kista.walk" (dict "node" $c "acc" $acc) -}}
{{- $azure := list -}}{{- $irsa := list -}}{{- $principals := list -}}
{{- range $acc.maps -}}
{{- if and (eq (toString .kind) "workload") (hasKey . "client_id") -}}{{- $azure = append $azure (toString .client_id) -}}{{- end -}}
{{- if eq (toString .kind) "irsa" -}}{{- $irsa = append $irsa (toString .role_arn) -}}{{- end -}}
{{- if and (hasKey . "principal") (not .credentials_file) -}}{{- $principals = append $principals (toString .principal) -}}{{- end -}}
{{- end -}}
{{- $id := $v.azure.workloadIdentity.clientId -}}
{{- if or (and $azure (not $id)) (and $id (not $azure)) -}}
{{- fail "azure.workloadIdentity.clientId and the config's workload identities (kind workload, client_id) go together" -}}
{{- end -}}
{{- range $azure -}}{{- if ne . $id -}}{{- fail (printf "config: workload identity client_id %s is not azure.workloadIdentity.clientId %s" . $id) -}}{{- end -}}{{- end -}}
{{- $role := $v.aws.irsa.roleArn -}}
{{- if or (and $irsa (not $role)) (and $role (not $irsa)) -}}
{{- fail "aws.irsa.roleArn and the config's irsa identities go together" -}}
{{- end -}}
{{- range $irsa -}}{{- if ne . $role -}}{{- fail (printf "config: irsa role_arn %s is not aws.irsa.roleArn %s" . $role) -}}{{- end -}}{{- end -}}
{{- $gsa := $v.gcp.serviceAccount -}}
{{- if and $gsa (not $principals) -}}
{{- fail "gcp.serviceAccount: nothing in config (gcpkms source, gcs domain, gcp database login) uses the metadata server" -}}
{{- end -}}
{{- if and $principals (not $gsa) -}}
{{- fail "config: a gcpkms source, gcs domain or the gcp database login uses the metadata server (no credentials_file): set gcp.serviceAccount (GKE Workload Identity)" -}}
{{- end -}}
{{- if $gsa -}}
{{- range $principals -}}{{- if ne . $gsa -}}{{- fail (printf "config: principal %s is not gcp.serviceAccount %s" . $gsa) -}}{{- end -}}{{- end -}}
{{- end -}}
{{- /* the NetworkPolicy */ -}}
{{- $np := $v.networkPolicy -}}
{{- $ic := $np.ingressController -}}
{{- if and (include "kista.networkPolicy" .) (not (or $ic.podSelector $np.loadBalancerRanges)) -}}
{{- fail "networkPolicy: name the ingress controller's pods (networkPolicy.ingressController.podSelector, with its namespaceSelector) or the load balancers (networkPolicy.loadBalancerRanges), or set networkPolicy.enabled: false" -}}
{{- end -}}
{{- if and $ic.namespaceSelector (not $ic.podSelector) -}}
{{- fail "networkPolicy.ingressController: a namespaceSelector alone admits every pod in those namespaces; add the controller's podSelector" -}}
{{- end -}}
{{- /* the egress NetworkPolicy (spec 0025): what the chart can tell kista needs */ -}}
{{- $eg := $np.egress -}}
{{- if $eg.enabled -}}
{{- $dnsPods := $eg.dns.podSelector | default dict -}}
{{- if and $eg.dns.enabled (or $eg.dns.podSelector $eg.dns.namespaceSelector) (not (or $dnsPods.matchLabels $dnsPods.matchExpressions)) -}}
{{- fail "networkPolicy.egress.dns: a podSelector without matchLabels or matchExpressions (or none, beside a namespaceSelector) admits every pod; name the DNS pods" -}}
{{- end -}}
{{- if and (not $sqlite) (not $eg.extraRules) -}}
{{- fail (printf "networkPolicy.egress: kista must reach its %s database; add a rule for it to networkPolicy.egress.extraRules" (toString $c.store.kind)) -}}
{{- end -}}
{{- with include "kista.proxy" . -}}
{{- $p := fromJsonArray . -}}
{{- if and (not (index $p 2)) (not $eg.extraRules) -}}
{{- fail (printf "networkPolicy.egress: config.egress.proxy names the host %s, which the chart cannot resolve; add a rule for the proxy to networkPolicy.egress.extraRules" (index $p 0)) -}}
{{- end -}}
{{- end -}}
{{- $kinds := list -}}
{{- range $acc.maps -}}{{- if hasKey . "kind" -}}{{- $kinds = append $kinds (toString .kind) -}}{{- end -}}{{- end -}}
{{- $need := dict -}}
{{- if $principals -}}{{- $_ := set $need "gke" "a gcpkms source, gcs domain or gcp database login without credentials_file (the GKE metadata server)" -}}{{- end -}}
{{- if has "pod_identity" $kinds -}}{{- $_ := set $need "aws-pod-identity" "an AWS identity of kind pod_identity (the EKS Pod Identity agent)" -}}{{- end -}}
{{- if or (has "imds" $kinds) (has "managed" $kinds) -}}{{- $_ := set $need "imds" "an identity of kind imds or managed (the instance metadata service)" -}}{{- end -}}
{{- range $name, $why := $need -}}
{{- if not (has $name $eg.cloudMetadata) -}}
{{- fail (printf "networkPolicy.egress: the config uses %s; add %s to networkPolicy.egress.cloudMetadata" $why $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* A Kubernetes quantity (bytes, or a number with Ki, Mi, Gi, Ti) in Mi, rounded up: (list name value). */}}
{{- define "kista.quantityMi" -}}
{{- $name := index . 0 -}}
{{- $v := index . 1 -}}
{{- /* a number in a values file is a float64, which toString writes as 1.073741824e+09 */ -}}
{{- if or (kindIs "float64" $v) (kindIs "int64" $v) (kindIs "int" $v) -}}{{- $v = int64 $v -}}{{- end -}}
{{- $s := toString $v -}}
{{- if not (regexMatch "^[0-9]{1,20}(Ki|Mi|Gi|Ti)?$" $s) -}}
{{- fail (printf "%s: a quantity like 2Gi (bytes, Ki, Mi, Gi or Ti), not %q" $name $s) -}}
{{- end -}}
{{- $n := float64 (regexFind "^[0-9]+" $s) -}}
{{- if hasSuffix "Ti" $s -}}{{- $n = mulf $n 1048576.0 -}}
{{- else if hasSuffix "Gi" $s -}}{{- $n = mulf $n 1024.0 -}}
{{- else if hasSuffix "Ki" $s -}}{{- $n = divf $n 1024.0 -}}
{{- else if not (hasSuffix "Mi" $s) -}}{{- $n = divf $n 1048576.0 -}}
{{- end -}}
{{- if gt $n 1000000000000.0 -}}
{{- fail (printf "%s: %s is too large" $name $s) -}}
{{- end -}}
{{- ceil $n | int -}}
{{- end -}}

{{/* The spool emptyDir's size: spool.sizeLimit, or the server's ingests, plus spool.importHeadroom. */}}
{{- define "kista.spoolSize" -}}
{{- $headroom := int (include "kista.quantityMi" (list "spool.importHeadroom" (.Values.spool.importHeadroom | default 0))) -}}
{{- if and .Values.spool.sizeLimit (eq $headroom 0) -}}
{{- .Values.spool.sizeLimit -}}
{{- else if .Values.spool.sizeLimit -}}
{{- printf "%dMi" (add (int (include "kista.quantityMi" (list "spool.sizeLimit" .Values.spool.sizeLimit))) $headroom) -}}
{{- else -}}
{{- $out := dict -}}{{- include "kista.userConfig" (dict "root" . "out" $out) -}}
{{- $c := $out.v -}}
{{- $body := int (include "kista.mib" (dig "blob" "max_body" "1GiB" $c)) -}}
{{- $ingests := int (dig "blob" "max_ingests" 4 $c) -}}
{{- if lt $ingests 1 -}}{{- $ingests = 4 -}}{{- end -}}
{{- printf "%dMi" (add (mul $body (add 1 (mul 2 $ingests))) $headroom) -}}
{{- end -}}
{{- end -}}

{{/* The container's resources, with an ephemeral-storage request for the spool unless set. */}}
{{- define "kista.resources" -}}
{{- $r := deepCopy (.Values.resources | default dict) -}}
{{- $req := get $r "requests" | default dict -}}
{{- if not (hasKey $req "ephemeral-storage") -}}
{{- $_ := set $req "ephemeral-storage" (include "kista.spoolSize" .) -}}
{{- end -}}
{{- $_ := set $r "requests" $req -}}
{{- toYaml $r -}}
{{- end -}}

{{- define "kista.grace" -}}
{{- if not (kindIs "invalid" .Values.terminationGracePeriodSeconds) -}}
{{- int .Values.terminationGracePeriodSeconds -}}
{{- else -}}
{{- $out := dict -}}{{- include "kista.userConfig" (dict "root" . "out" $out) -}}
{{- $c := $out.v -}}
{{- $drain := int (include "kista.seconds" (dig "serve" "drain_timeout" "5s" $c)) -}}
{{- $shut := int (include "kista.seconds" (dig "serve" "shutdown_timeout" "30s" $c)) -}}
{{- add (int .Values.preStopSeconds) $drain (mul 2 $shut) 21 10 -}}
{{- end -}}
{{- end -}}

{{/* The pod's volumes and mounts, shared by the Deployment and the admin CronJobs. */}}
{{- define "kista.volumes" -}}
- name: config
  configMap:
    name: {{ include "kista.fullname" . }}
- name: tmp
  emptyDir:
    sizeLimit: {{ include "kista.spoolSize" . }}
{{- if include "kista.sqlite" . }}
- name: data
  persistentVolumeClaim:
    claimName: {{ .Values.persistence.existingClaim | default (include "kista.name" (list . "data")) }}
{{- end }}
{{- if .Values.blobVolume.enabled }}
- name: blobs
  persistentVolumeClaim:
    claimName: {{ .Values.blobVolume.existingClaim | default (include "kista.name" (list . "blobs")) }}
{{- end }}
{{- if eq .Values.mode "tls" }}
- name: tls
  secret:
    secretName: {{ .Values.tlsSecret }}
    defaultMode: 0440
{{- end }}
{{- range .Values.secretVolumes }}
- name: secret-{{ .name }}
  secret:
    secretName: {{ .secretName }}
    defaultMode: 0440
    {{- with .items }}
    items: {{- toYaml . | nindent 6 }}
    {{- end }}
{{- end }}
{{- range .Values.projectedTokens }}
- name: token-{{ .name }}
  projected:
    sources:
      - serviceAccountToken:
          audience: {{ .audience | quote }}
          expirationSeconds: {{ int (.expirationSeconds | default 3600) }}
          path: token
{{- end }}
{{- end -}}

{{- define "kista.mounts" -}}
- name: config
  mountPath: /etc/kista/kista.yaml
  subPath: kista.yaml
  readOnly: true
- name: tmp
  mountPath: /var/lib/kista/tmp
{{- if include "kista.sqlite" . }}
- name: data
  mountPath: /var/lib/kista/data
{{- end }}
{{- if .Values.blobVolume.enabled }}
- name: blobs
  mountPath: /var/lib/kista/blobs
{{- end }}
{{- if eq .Values.mode "tls" }}
- name: tls
  mountPath: /etc/kista/tls
  readOnly: true
{{- end }}
{{- range .Values.secretVolumes }}
- name: secret-{{ .name }}
  mountPath: {{ .mountPath }}
  readOnly: true
{{- end }}
{{- range .Values.projectedTokens }}
- name: token-{{ .name }}
  mountPath: {{ .mountPath }}
  readOnly: true
{{- end }}
{{- end -}}

{{- define "kista.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
fsGroupChangePolicy: OnRootMismatch
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "kista.securityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: [ALL]
{{- end -}}

{{- define "kista.env" -}}
- name: TMPDIR
  value: /var/lib/kista/tmp
{{- with .Values.env }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* The namespace of every namespaced object: namespaceOverride, else the release's (spec 0024). */}}
{{- define "kista.namespace" -}}
{{- .Values.namespaceOverride | default .Release.Namespace -}}
{{- end -}}
