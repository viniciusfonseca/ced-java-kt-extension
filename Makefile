PACKAGE := dist/viniciusfonseca.java-kotlin
TARGETS := darwin-arm64 darwin-x86_64 linux-arm64 linux-x86_64
BINARIES := $(foreach target,$(TARGETS),$(PACKAGE)/bin/$(target)/java-kotlin)
SOURCE := $(filter-out %_test.go,$(wildcard *.go))

.PHONY: package test test-ced
package: $(BINARIES) $(addprefix $(PACKAGE)/,ced-extension.json README.md TESTING.md LICENSE)
	@echo "CED package: $(abspath $(PACKAGE))"

$(PACKAGE)/bin/%/java-kotlin: $(SOURCE) go.mod Makefile
	mkdir -p $(@D)
	CGO_ENABLED=0 GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(subst x86_64,amd64,$(word 2,$(subst -, ,$*))) go build -trimpath -buildvcs=false -o $@ .

$(PACKAGE)/%: %
	mkdir -p $(@D)
	cp $< $@

test: package
	go test -race ./...

CED_SOURCE ?= ../ced
CED_BUILD ?= $(CED_SOURCE)/build-extensions.noindex
test-ced: package
	./tests/test-ced.sh "$(CED_SOURCE)" "$(CED_BUILD)" "$(abspath $(PACKAGE))"
