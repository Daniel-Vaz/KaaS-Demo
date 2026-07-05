.PHONY: help up down logs ps restart rebuild psql nuke up-fake down-fake logs-fake \
        up-scale down-scale logs-scale ps-scale helm-lint helm-template images images-push \
        catalog-check catalog-update \
        _clusters-down build test vet run-api run-worker golden-image golden-image-vsphere golden-image-proxmox golden-images tidy clean \
        web-install web-dev web-build kubeconfig

# Container lifecycle is driven by Podman Compose.
COMPOSE      ?= podman compose
# Default = REAL mode (host-networked worker drives real VMs). Fake mode is base only.
#
# compose.ports.yaml publishes the host ports (:8080 portal, :8081 API) and so pins web and api to
# ONE replica each - which is what the single-replica targets want. The scaled target swaps it for
# compose.scale.yaml, where a load balancer owns those ports and fans out over the replicas.
REAL         := -f deploy/compose.yaml -f deploy/compose.real.yaml -f deploy/compose.ports.yaml
FAKE         := -f deploy/compose.yaml -f deploy/compose.ports.yaml
SCALE        := -f deploy/compose.yaml -f deploy/compose.real.yaml -f deploy/compose.scale.yaml

# Replica counts for `make up-scale` (override on the command line: make up-scale API=3 WORKER=4).
# Postgres is deliberately NOT scaled - it is the single source of truth, and real HA Postgres is
# out of scope (docs/architecture.md).
WEB          ?= 2
API          ?= 2
WORKER       ?= 2
# HashiCorp packer (PATH often shadows it with cracklib's /usr/sbin/packer).
PACKER       ?= /usr/bin/packer

help: ## Show this help
	@echo "KaaS-Demo - common targets:"
	@echo ""
	@echo "  Containers - REAL KVM mode (default; needs libvirt + a .env, see .env.example):"
	@echo "    make up          Build + start web + api + postgres + host-networked worker (real VMs)"
	@echo "    make down        Stop and remove the containers (keeps the DB volume)"
	@echo "    make logs        Follow logs (incl. the worker)"
	@echo "    make ps          Show container status"
	@echo "    make restart     Recreate the stack"
	@echo "    make rebuild     Rebuild images and recreate"
	@echo "    make psql        Open a psql shell in the postgres container"
	@echo "    make nuke        Stop and remove containers AND the DB volume"
	@echo "    make kubeconfig CLUSTER=<id>   Fetch a cluster's admin kubeconfig -> /tmp/kubeconfig"
	@echo ""
	@echo "  Containers - SCALED (real mode, N replicas behind a load balancer):"
	@echo "    make up-scale    Start web/api/worker with 2 replicas each (WEB=3 API=3 WORKER=4 to override)"
	@echo "    make ps-scale    Show every replica"
	@echo "    make down-scale  Stop and remove the scaled stack"
	@echo ""
	@echo "  Containers - FAKE mode (no KVM, no .env; the API reconciles in-process):"
	@echo "    make up-fake     Start web + api + postgres with fake providers (test the portal)"
	@echo "    make down-fake   Stop the fake-mode stack"
	@echo "    make logs-fake   Follow fake-mode logs"
	@echo ""
	@echo "  Portal: http://localhost:8080 (nginx serves the SPA + proxies /api to the API on :8081)"
	@echo ""
	@echo "  Web portal dev (needs Node 18+; hot-reload against a local 'make run-api'):"
	@echo "    make web-install Install portal dependencies (npm ci in web/portal)"
	@echo "    make web-dev     Vite dev server on :5173, proxying /api -> localhost:8080"
	@echo "    make web-build   Production build of the portal (type-check + vite build)"
	@echo ""
	@echo "  Kubernetes (Helm chart, deploy/helm/kaas - N replicas of every tier):"
	@echo "    make images-push REGISTRY=...  Build + push the api/web/worker/shell images"
	@echo "    make helm-lint                 Lint + render the chart in all modes"
	@echo "    helm install kaas deploy/helm/kaas --set providers=fake   (no hypervisor needed)"
	@echo ""
	@echo "  Catalog add-on versions (internal/catalog/catalog.json, needs helm + python3 on the host):"
	@echo "    make catalog-check   Report add-ons with a newer chart version upstream (no changes)"
	@echo "    make catalog-update  Rewrite catalog.json to the latest upstream chart versions"
	@echo ""
	@echo "  Local dev (no containers):"
	@echo "    make build test vet | run-api | run-worker | tidy | clean"
	@echo "    make golden-image [OS_NAME=.. K8S_VERSION=.. BASE_IMAGE_URL=..]  Bake one golden image"
	@echo "    make golden-images   Bake the shipped catalog image (ubuntu-26.04 k8s 1.36.2; kvm + vSphere + Proxmox)"
	@echo "    make golden-image-vsphere [OS_NAME=.. K8S_VERSION=..]  Bake the vSphere VM template"
	@echo "    make golden-image-proxmox [OS_NAME=.. K8S_VERSION=..]  Bake the Proxmox VM template"

# ---- Containers: REAL KVM mode (default) -----------------------------------------------

up: ## Build + start the real-mode stack (web + api + postgres + host-networked worker)
	$(COMPOSE) $(REAL) up -d --build
	@echo ""
	@echo "  Portal at http://localhost:8080 (API direct on :8081); the worker drives real VMs."
	@echo "  Watch it: make logs   Tear down: make down"

down: _clusters-down ## Delete running clusters, then stop + remove the containers (keeps pgdata)
	$(COMPOSE) $(REAL) down
	podman volume prune -f

logs:
	$(COMPOSE) $(REAL) logs -f

ps:
	$(COMPOSE) $(REAL) ps

restart: ## Bounce the containers (keeps clusters and the DB)
	$(COMPOSE) $(REAL) restart

rebuild: ## Rebuild images and recreate the stack
	$(COMPOSE) $(REAL) up -d --build --force-recreate

psql: ## psql shell into the postgres container
	$(COMPOSE) $(FAKE) exec postgres psql -U kaas -d kaas

nuke: _clusters-down ## Delete running clusters, then remove containers AND the DB volume
	$(COMPOSE) $(REAL) down -v

# Best-effort: ask the running API to delete all clusters and wait for their VMs to be torn
# down BEFORE the containers are removed - otherwise stopping the worker mid-destroy orphans
# libvirt domains. A no-op if the API isn't running (e.g. already down). See the script.
_clusters-down:
	-@bash deploy/teardown-clusters.sh

# Fetch a cluster's kubeconfig via the running API and write it to /tmp/kubeconfig. Clusters are
# owner-scoped (GET /clusters/{id}/kubeconfig needs a session), so this logs in as the seeded admin
# (default admin/admin; override with KAAS_ADMIN_USERNAME/KAAS_ADMIN_PASSWORD) and reuses that
# session's cookie - the admin gets the cluster-admin credential. Local execution against the API
# published on :8081 (override with KAAS_API). Pass the cluster id: make kubeconfig CLUSTER=<id>
KAAS_API      ?= http://localhost:8081
KUBECONFIG_OUT ?= /tmp/kubeconfig
kubeconfig: ## Fetch a cluster's admin kubeconfig to /tmp/kubeconfig (needs CLUSTER=<id>)
	@if [ -z "$(CLUSTER)" ]; then echo "kubeconfig: set the cluster id, e.g. make kubeconfig CLUSTER=<id>" >&2; exit 1; fi
	@jar="$$(mktemp)"; trap 'rm -f "$$jar"' EXIT; \
	 user="$${KAAS_ADMIN_USERNAME:-admin}"; pass="$${KAAS_ADMIN_PASSWORD:-admin}"; \
	 if ! curl -sf -o /dev/null -c "$$jar" -X POST "$(KAAS_API)/auth/login" \
	        -H 'Content-Type: application/json' \
	        -d "{\"username\":\"$$user\",\"password\":\"$$pass\"}"; then \
	   echo "kubeconfig: admin login failed (API reachable at $(KAAS_API)?)" >&2; exit 1; fi; \
	 if ! curl -sf -b "$$jar" "$(KAAS_API)/clusters/$(CLUSTER)/kubeconfig" -o "$(KUBECONFIG_OUT)"; then \
	   echo "kubeconfig: fetch failed for cluster $(CLUSTER) (does it exist and is it Ready?)" >&2; exit 1; fi; \
	 echo "kubeconfig: wrote $(KUBECONFIG_OUT) (use it with: KUBECONFIG=$(KUBECONFIG_OUT) kubectl get nodes)"

# ---- Containers: horizontally scaled (real mode, N replicas behind a load balancer) -----
#
# Same stack as `make up`, with several replicas of web, api and worker, plus a second exec-agent
# sandbox. See deploy/compose.scale.yaml for WHY each tier is safe to replicate (durable
# unique-per-cluster job queue, leader-elected singleton loops, advisory-locked admission,
# stateless exec agents). The addresses are unchanged: portal on :8080, API on :8081.

# Counts are EXPORTED, not passed as --scale: the compose files carry a per-service `scale:` key
# that interpolates them (see deploy/compose.scale.yaml - podman-compose takes only one --scale
# flag per run, and using it would force every other service back to a single replica).
up-scale: export WEB := $(WEB)
up-scale: export API := $(API)
up-scale: export WORKER := $(WORKER)
up-scale: ## Build + start the scaled stack (override counts: make up-scale WEB=3 API=3 WORKER=4)
	$(COMPOSE) $(SCALE) up -d --build
	@echo ""
	@echo "  Scaled up: web=$(WEB) api=$(API) worker=$(WORKER) (+ 2 exec agents, 1 postgres)."
	@echo "  Portal at http://localhost:8080, API on :8081 - both via the lb container."
	@echo "  Watch it: make logs-scale   Tear down: make down-scale"

down-scale: _clusters-down ## Delete clusters, then stop + remove the scaled stack (keeps pgdata)
	$(COMPOSE) $(SCALE) down
	podman volume prune -f

logs-scale:
	$(COMPOSE) $(SCALE) logs -f

ps-scale: ## Show the scaled stack's containers (one line per replica)
	$(COMPOSE) $(SCALE) ps

# ---- Kubernetes: the Helm chart (deploy/helm/kaas) --------------------------------------
#
# The same four images, deployed on a real cluster with N replicas of each. See
# deploy/helm/kaas/README.md. `providers=fake` needs no hypervisor and is the way to try it.

HELM         ?= helm
CHART        := deploy/helm/kaas
REGISTRY     ?= localhost/kaas
IMAGE_TAG    ?= latest

helm-lint: ## Lint the chart and render it in every mode (fake / real-remote / real-local)
	$(HELM) lint $(CHART)
	@$(HELM) template kaas $(CHART) --set providers=fake >/dev/null && echo "  rendered: providers=fake"
	@$(HELM) template kaas $(CHART) --set kvm.host=10.0.0.1 --set config.clusterSSH.publicKey=x >/dev/null \
		&& echo "  rendered: real / kvm.mode=remote"
	@$(HELM) template kaas $(CHART) --set kvm.mode=local --set config.clusterSSH.publicKey=x >/dev/null \
		&& echo "  rendered: real / kvm.mode=local"

helm-template: ## Render the chart to stdout (override with any --set via HELM_ARGS)
	$(HELM) template kaas $(CHART) $(HELM_ARGS)

# ---- Catalog add-on versions (internal/catalog/catalog.json) --------------------------

catalog-check: ## Report which catalog add-on charts have a newer version upstream (no changes)
	python3 scripts/update-catalog-versions.py --check

catalog-update: ## Rewrite catalog.json add-on entries to the latest upstream chart versions
	python3 scripts/update-catalog-versions.py --write

images: ## Build the container images for a registry (REGISTRY=... IMAGE_TAG=...)
	for c in api web worker shell nodessh; do \
		podman build -f deploy/Containerfile.$$c -t $(REGISTRY)/$$c:$(IMAGE_TAG) . || exit 1; \
	done
	@echo "  built $(REGISTRY)/{api,web,worker,shell,nodessh}:$(IMAGE_TAG)"

images-push: images ## Build and push the images the chart pulls
	for c in api web worker shell nodessh; do podman push $(REGISTRY)/$$c:$(IMAGE_TAG) || exit 1; done

# ---- Containers: FAKE mode (no KVM) ----------------------------------------------------

up-fake: ## Build + start the fake-mode stack (web + api + postgres, no worker)
	$(COMPOSE) $(FAKE) up -d --build
	@echo ""
	@echo "  Portal ready at http://localhost:8080 (API direct on :8081; fake providers)."
	@echo "  Watch it: make logs-fake   Tear down: make down-fake"

down-fake: _clusters-down ## Delete clusters, then stop + remove the fake-mode containers (keeps pgdata)
	$(COMPOSE) $(FAKE) down
	podman volume prune -f

logs-fake:
	$(COMPOSE) $(FAKE) logs -f

# ---- Web portal (React + Mantine SPA in web/portal) ------------------------------------
# `make up` / `up-fake` build and run the portal in a container automatically. These targets
# are for iterating on the UI locally with hot reload (needs Node 18+ on the host).

WEB_DIR := web/portal

web-install: ## Install portal dependencies (npm ci)
	cd $(WEB_DIR) && npm ci

web-dev: ## Vite dev server on :5173, proxying /api -> localhost:8080 (run `make run-api` alongside)
	cd $(WEB_DIR) && npm run dev

web-build: ## Production build of the portal (type-check + vite build)
	cd $(WEB_DIR) && npm run build

# ---- Local dev (no containers) ---------------------------------------------------------

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

# Run the API + reconciler together (create clusters over REST, watch via SSE).
run-api:
	go run ./cmd/api

# Run the reconciler headless in demo mode: seeds a cluster and logs it converging to Ready.
run-worker:
	KAAS_SEED_DEMO=1 go run ./cmd/worker

# Golden image build parameters. Override on the command line to bake a different (OS, k8s):
#   make golden-image OS_NAME=ubuntu-24.04 K8S_VERSION=1.36.2 BASE_IMAGE_URL=<noble cloud qcow2>
# The image filename follows catalog.GoldenImageName: <OS_NAME>-k8s-<K8S_VERSION>.qcow2. Built
# images land in GOLDEN_DEST (libvirt's default pool dir), which is the KAAS_IMAGE_DIR the
# provisioner reads. Override GOLDEN_DEST to write elsewhere (e.g. packer/output for local runs).
OS_NAME        ?= ubuntu-26.04
K8S_VERSION    ?= 1.36.2
BASE_IMAGE_URL ?= https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.img
GOLDEN_DEST    ?= /var/lib/libvirt/images
IMAGE_NAME      = $(OS_NAME)-k8s-$(K8S_VERSION).qcow2

# Build one golden VM image with Packer (needs KVM; downloads the base cloud image). Skips the
# build entirely if $(GOLDEN_DEST)/$(IMAGE_NAME) already exists - a ~10min KVM boot, not worth
# repeating. Force a rebuild by removing that file. Otherwise: wipes packer/build/ (Packer's temp
# output_dir), builds, then moves the finished image into GOLDEN_DEST (auto-elevating with sudo if
# that dir isn't writable, since /var/lib/libvirt/images is usually root-owned).
golden-image:
	@if [ -f "$(GOLDEN_DEST)/$(IMAGE_NAME)" ]; then \
	   echo "golden-image: $(GOLDEN_DEST)/$(IMAGE_NAME) already exists - skipping build (rm it to force a rebuild)"; \
	   exit 0; \
	 fi; \
	 rm -rf packer/build; \
	 (cd packer && $(PACKER) init . && $(PACKER) build \
	    -var k8s_version=$(K8S_VERSION) \
	    -var os_name=$(OS_NAME) \
	    -var base_image_url='$(BASE_IMAGE_URL)' \
	    -var output_name=$(IMAGE_NAME) .) || exit 1; \
	 built="$$(ls packer/build/*.qcow2 2>/dev/null | head -1)"; \
	 if [ -z "$$built" ]; then echo "golden-image: no .qcow2 produced in packer/build" >&2; exit 1; fi; \
	 dest="$(GOLDEN_DEST)/$(IMAGE_NAME)"; \
	 if mkdir -p "$(GOLDEN_DEST)" 2>/dev/null && mv "$$built" "$$dest" 2>/dev/null; then :; \
	 else echo "golden-image: $(GOLDEN_DEST) needs elevated write - using sudo"; \
	      sudo mkdir -p "$(GOLDEN_DEST)" && sudo mv "$$built" "$$dest"; fi; \
	 echo "golden-image: moved -> $$dest"

# Build the full set of golden images the shipped catalog references (skips any that already
# exist): the KVM qcow2 always, plus the vSphere VM template too if KAAS_VSPHERE_* is configured
# (source .env first) - mirroring KAAS_INFRA_PROVIDERS, vSphere is opt-in, so its absence doesn't
# fail the KVM-only (default) path. The KVM image is the head/default new clusters boot.
golden-images:
	$(MAKE) golden-image OS_NAME=ubuntu-26.04 K8S_VERSION=1.36.2 BASE_IMAGE_URL='https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.img'
	@if [ -n "$$KAAS_VSPHERE_FOLDER" ]; then \
	   $(MAKE) golden-image-vsphere OS_NAME=ubuntu-26.04 K8S_VERSION=1.36.2; \
	 else \
	   echo "golden-images: KAAS_VSPHERE_* not set - skipping the vSphere template (source .env and re-run, or use 'make golden-image-vsphere' directly)"; \
	 fi
	@if [ -n "$$KAAS_PROXMOX_ENDPOINT" ]; then \
	   $(MAKE) golden-image-proxmox OS_NAME=ubuntu-26.04 K8S_VERSION=1.36.2; \
	 else \
	   echo "golden-images: KAAS_PROXMOX_* not set - skipping the Proxmox template (source .env and re-run, or use 'make golden-image-proxmox' directly)"; \
	 fi

# ---- Golden VM templates (vSphere) ------------------------------------------------------
#
# The vSphere equivalent of golden-image: bakes the same node software into a vCenter VM TEMPLATE
# named <OS_NAME>-k8s-<K8S_VERSION> (no suffix - catalog.GoldenImageNameFor for vsphere) in the
# KAAS_VSPHERE_FOLDER, which the infra/vsphere module clones per node.
#
#   source .env && make golden-image-vsphere [OS_NAME=.. K8S_VERSION=.. SEED_TEMPLATE=..]
#
# Connection/placement come from the KAAS_VSPHERE_* environment (the same values the worker uses).
# Requires the seed template imported once per OS - see docs/infrastructure.md for the `govc
# import.ova` invocation. Skips the build if the template already exists.
#
# After Packer builds it, cmd/vsphere-prep strips the vApp/OVF config the template inherits from
# the Ubuntu OVA seed. That step is NOT optional: without it the OpenTofu vSphere provider refuses
# to clone the template ("requires a client CDROM device to deliver vApp properties"), and papering
# over that with a CD-ROM would let cloud-init's OVF datasource win over the guestinfo one and boot
# every node with the template's EMPTY user-data. See cmd/vsphere-prep/main.go.
SEED_TEMPLATE  ?= ubuntu-26.04-cloudimg-seed
TEMPLATE_NAME   = $(OS_NAME)-k8s-$(K8S_VERSION)
GOVC           ?= govc

golden-image-vsphere:
	@if [ -z "$$KAAS_VSPHERE_FOLDER" ]; then \
	   echo "golden-image-vsphere: KAAS_VSPHERE_* is not set - source your .env first" >&2; exit 1; \
	 fi; \
	 export GOVC_URL="$$KAAS_VSPHERE_URL" GOVC_USERNAME="$$KAAS_VSPHERE_USERNAME" \
	        GOVC_PASSWORD="$$KAAS_VSPHERE_PASSWORD" GOVC_INSECURE="$${KAAS_VSPHERE_INSECURE:-0}" \
	        GOVC_DATACENTER="$$KAAS_VSPHERE_DATACENTER"; \
	 if $(GOVC) vm.info "$$KAAS_VSPHERE_FOLDER/$(TEMPLATE_NAME)" 2>/dev/null | grep -q "Name:"; then \
	   echo "golden-image-vsphere: template $$KAAS_VSPHERE_FOLDER/$(TEMPLATE_NAME) already exists - skipping (govc vm.destroy it to force a rebuild)"; \
	   exit 0; \
	 fi; \
	 export KAAS_VSPHERE_SERVER="$$(echo "$$KAAS_VSPHERE_URL" | sed -e 's#^https\?://##' -e 's#/.*$$##')"; \
	 (cd packer/vsphere && $(PACKER) init . && $(PACKER) build \
	    -var k8s_version=$(K8S_VERSION) \
	    -var os_name=$(OS_NAME) \
	    -var seed_template='$(SEED_TEMPLATE)' \
	    -var output_name=$(TEMPLATE_NAME) .) || exit 1; \
	 go run ./cmd/vsphere-prep -template $(TEMPLATE_NAME) || exit 1; \
	 echo "golden-image-vsphere: built template $$KAAS_VSPHERE_FOLDER/$(TEMPLATE_NAME)"

# ---- Golden VM templates (Proxmox) ------------------------------------------------------
#
# The Proxmox equivalent of golden-image: bakes the same node software into a Proxmox VM TEMPLATE
# named <OS_NAME>-k8s-<K8S_VERSION> (no suffix - catalog.GoldenImageNameFor for proxmox) on
# KAAS_PROXMOX_NODE, which the infra/proxmox module clones per node. The template also ships
# qemu-guest-agent (how Proxmox reports a node's DHCP address back).
#
#   source .env && make golden-image-proxmox [OS_NAME=.. K8S_VERSION=.. SEED_TEMPLATE=..]
#
# Connection/placement come from the KAAS_PROXMOX_* environment (the same values the worker uses).
# Requires the seed template created once per OS - see docs/infrastructure.md / the packer file's
# header for the `qm` commands. Unlike vSphere there is no post-build prep step (Proxmox has no vApp
# config to strip). Proxmox rejects a duplicate template name, so destroy an existing one to rebuild.
golden-image-proxmox:
	@if [ -z "$$KAAS_PROXMOX_ENDPOINT" ]; then \
	   echo "golden-image-proxmox: KAAS_PROXMOX_* is not set - source your .env first" >&2; exit 1; \
	 fi; \
	 (cd packer/proxmox && $(PACKER) init . && $(PACKER) build \
	    -var k8s_version=$(K8S_VERSION) \
	    -var os_name=$(OS_NAME) \
	    -var seed_template='$(SEED_TEMPLATE)' \
	    -var output_name=$(TEMPLATE_NAME) .) || exit 1; \
	 echo "golden-image-proxmox: built template $(TEMPLATE_NAME) on node $$KAAS_PROXMOX_NODE"

tidy:
	go mod tidy

clean:
	go clean ./...
