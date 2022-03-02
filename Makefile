VERSION_MAJOR ?= 0
VERSION_MINOR ?= 0
VERSION_BUILD ?= 0-ci-dev

VERSION ?= v$(VERSION_MAJOR).$(VERSION_MINOR).$(VERSION_BUILD)

SHELL := /bin/bash
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
REGISTRY ?= tmaxcloudck/stonelb

GO_FILES := $(shell find . -type f -name '*.go' -not -path "./vendor/*")
BUILD_ARG ?=

export GO111MODULE = on
export GOFLAGS = -mod=vendor

stonelb: $(GO_FILES)
	GOARCH=$(GOARCH) GOOS=linux CGO_ENABLED=0 go build -o $@

.PHONY: image
image:
	docker build -t $(REGISTRY):$(VERSION) -f build/package/Dockerfile .

.PHONY: push
push:
	docker push $(REGISTRY):$(VERSION)