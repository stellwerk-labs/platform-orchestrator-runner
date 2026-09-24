KUBECONFIG := runner-cluster/kubeconfig.yaml
KUBECONFIG_INTERNAL := runner-cluster/kubeconfig_internal.yaml
KUBECTL := kubectl --kubeconfig $(KUBECONFIG)
KUBECTL_INTERNAL := kubectl --kubeconfig $(KUBECONFIG_INTERNAL)
CLUSTER_NAME := kind-runner
NAME := runner
DEFAULT_K8S_RES := platform-orchestrator-runner
RUNNER_CONFIGURATION := runner_config.json

CP_IMAGE ?= ghcr.io/stellwerk-labs/platform-orchestrator-cp:v2.0.0
DP_IMAGE ?= ghcr.io/stellwerk-labs/platform-orchestrator-dp:v3.0.0
IAM_IMAGE ?= ghcr.io/stellwerk-labs/platform-orchestrator-iam:v2.1.0
SERVICE_PLATFORM ?=
PULL_FLAGS ?=

compose.yaml: Makefile compose-mixins.yaml score.provisioners.yaml score.cp.yaml score.dp.yaml score.iam.yaml
	score-compose check-version '>=0.16.0'
	score-compose init --no-sample
	cp score.provisioners.yaml .score-compose/
	cp score.cp.yaml score.dp.yaml score.iam.yaml .score-compose/
	yq -i '.containers.platform-orchestrator-cp.image = "$(CP_IMAGE)"' .score-compose/score.cp.yaml
	yq -i '.containers.platform-orchestrator-dp.image = "$(DP_IMAGE)"' .score-compose/score.dp.yaml
	yq -i '.containers.platform-orchestrator-iam.image = "$(IAM_IMAGE)"' .score-compose/score.iam.yaml
	score-compose generate .score-compose/score.dp.yaml
	score-compose generate .score-compose/score.cp.yaml
	score-compose generate .score-compose/score.iam.yaml
	score-compose generate --publish 5432:postgres.default#platform-orchestrator-dp.postgres.host:5432 --publish 8081:platform-orchestrator-cp:8080 --publish 8082:platform-orchestrator-dp:8080 --publish 8083:platform-orchestrator-iam:8080
	yq eval-all '. as $$item ireduce ({}; . * $$item)' --inplace compose.yaml compose-mixins.yaml
	if [ -n "$(SERVICE_PLATFORM)" ]; then yq -i '(.services.platform-orchestrator-cp-platform-orchestrator-cp.platform, .services.platform-orchestrator-dp-platform-orchestrator-dp.platform, .services.platform-orchestrator-iam-platform-orchestrator-iam.platform) = "$(SERVICE_PLATFORM)"' compose.yaml; fi

.PHONY: build-platform-orchestrator-runner
build-platform-orchestrator-runner:
	docker build -t stellwerk-labs/platform-orchestrator-runner ../.
	docker tag stellwerk-labs/platform-orchestrator-runner stellwerk-labs/platform-orchestrator-runner:test
	
.PHONY: build
build: compose.yaml build-platform-orchestrator-runner
	docker compose pull $(PULL_FLAGS)
	docker compose build --pull

.PHONY: test
test:
	docker compose up --no-build -d platform-orchestrator-cp-platform-orchestrator-cp platform-orchestrator-dp-platform-orchestrator-dp --wait
	timeout 60s sh -c 'until curl --fail --silent --output /dev/null http://127.0.0.1:8081/health && curl --fail --silent --output /dev/null http://127.0.0.1:8082/health; do sleep 1; done'
	docker network connect integration-tests_default runner-control-plane || true
	SERVER_URL="http://$(shell score-compose resources get-outputs dns.default\#api-dns | jq -r .host):8080" INTERNAL_CP_URL=http://localhost:8081 DB_CONNECTION_STRING="$(shell score-compose resources get-outputs 'postgres.default#platform-orchestrator-dp.postgres' -f 'host=localhost port=5432 dbname={{ .name }} user={{ .username }} password={{ .password }} connect_timeout=5 sslmode=disable')" RUNNER_TOKEN_SALT="vnXd0Ses8L1X0TGGcp+mf/V8dQs+L4/fWvijORwb7lI=" go tool gotestsum --format testname ./ -test.timeout 10m -test.parallel=2 -test.v $(TESTARGS)

.PHONY: prepare-tests
prepare-tests: build create-cluster write-cluster-config configure-cluster

## create a test cluster
.PHONY: create-cluster
create-cluster:
	kind create cluster --name $(NAME) --wait 5m --kubeconfig $(KUBECONFIG)


## create kubeconfig to access runner cluster
.PHONY: write-cluster-config
write-cluster-config:
	kind export kubeconfig --name $(NAME) --kubeconfig $(KUBECONFIG)
	kind export kubeconfig --name $(NAME) --internal --kubeconfig $(KUBECONFIG_INTERNAL)

.PHONY: configure-cluster
configure-cluster:
	$(KUBECTL) delete namespace $(DEFAULT_K8S_RES) --ignore-not-found
	$(KUBECTL) create namespace $(DEFAULT_K8S_RES)
	kind load docker-image --name ${NAME} stellwerk-labs/platform-orchestrator-runner:test
	$(KUBECTL) apply -f runner-cluster/config -R
	echo "$${RUNNER_GIT_SSH_KEYS}" | $(KUBECTL) create secret generic -n $(DEFAULT_K8S_RES) runner-git-ssh-keys --from-file=key=/dev/stdin || true
	$(KUBECTL) create secret generic -n $(DEFAULT_K8S_RES) aws-creds --from-literal=access_key_id="FAKE_ACCESS_KEY_ID" --from-literal=secret_access_key="SECRET_ACCESS_KEY"
	@printf '{"cluster_data":{"certificate_authority_data": "%s", "server": "%s"},"auth":{"client_certificate_data": "%s", "client_key_data": "%s"}}\n' "$$($(KUBECTL_INTERNAL) config view -o jsonpath='{.clusters[?(@.name == "$(CLUSTER_NAME)")].cluster.certificate-authority-data}' --raw )" "$$($(KUBECTL_INTERNAL) config view -o jsonpath='{.clusters[?(@.name == "$(CLUSTER_NAME)")].cluster.server}' --raw )" "$$( $(KUBECTL_INTERNAL) config view -o jsonpath='{.users[?(@.name == "$(CLUSTER_NAME)")].user.client-certificate-data}' --raw )" "$$( $(KUBECTL_INTERNAL) config view -o jsonpath='{.users[?(@.name == "$(CLUSTER_NAME)")].user.client-key-data}' --raw )" > $(RUNNER_CONFIGURATION)

.PHONY: clean
clean:
	if [ -f compose.yaml ]; then docker compose down -v --remove-orphans; fi
	rm -rfv .kind-e2e .score-compose compose.yaml
	kind delete cluster --name $(NAME) --kubeconfig $(KUBECONFIG)

.PHONY: test-integration-logs
test-integration-logs:
	docker compose logs
