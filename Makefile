.PHONY: test build acceptance ledger site-build site-contract-check

SITE_PYTHON ?= python3

test:
	go test -race ./...
	mvn -B -q -f maven-plugin/pom.xml verify
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s acceptance -p '*_test.py' -v
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s integration-tests/live-v2 -p 'test_*.py' -v
	python3 acceptance/check.py

build:
	mkdir -p bin
	go build -o bin/brewlet ./cmd/brewlet

ledger:
	python3 acceptance/check.py

acceptance:
	python3 acceptance/check.py --require-complete

site-contract-check:
	PYTHONDONTWRITEBYTECODE=1 $(SITE_PYTHON) -m unittest discover -s site/scripts -p 'test_*.py' -v
	node --check site/assets/js/site.js

site-build: site-contract-check
	$(SITE_PYTHON) site/scripts/build.py
