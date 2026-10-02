.PHONY: lab lab-health lab-cleanup contracts test build-cloudshop build-control-plane

lab:
	docker compose up -d

lab-health:
	./scripts/lab-health.sh

lab-cleanup:
	./scripts/lab-cleanup.sh

contracts:
	python3 scripts/validate_contracts.py --all

test:
	python3 -m pytest tests/contract -q

build-cloudshop:
	docker build -f workloads/cloudshop/Dockerfile workloads/cloudshop -t skybridge/cloudshop:dev

build-control-plane:
	docker build -f apps/control-plane/Dockerfile apps/control-plane -t skybridge/control-plane:dev
