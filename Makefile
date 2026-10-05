# WB2Hub Lite — build helpers.
#
# Two artifacts come out of this repo:
#   gateway : the Go service, cross-compiled to an Android arm64 binary and
#             dropped into jniLibs so the APK can exec it (see app/build.gradle).
#   apk     : the Android app that hosts and supervises that service.
#
# `make apk` depends on `make gateway`, because packaging a stale gateway is the
# failure mode this whole layout exists to prevent.

GO          ?= go
GO_SRC      := go-src
GATEWAY_OUT := app/src/main/jniLibs/arm64-v8a/libwb2hub.so
VERSION     ?= 1.0.0

GRADLE      ?= gradle
ANDROID_HOME ?= $(HOME)/Android/Sdk
JAVA_HOME   ?= /usr/lib/jvm/java-17-openjdk-arm64

export ANDROID_HOME
export JAVA_HOME

.PHONY: all gateway apk test vet fmt clean version

all: apk

## gateway: cross-compile the Go service for Android arm64.
##
## CGO_ENABLED=0 produces a static binary with no libc dependency, which is what
## lets it run on Android without an NDK sysroot.
gateway:
	@mkdir -p $(dir $(GATEWAY_OUT))
	cd $(GO_SRC) && CGO_ENABLED=0 GOOS=android GOARCH=arm64 \
		$(GO) build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" \
		-o ../../$(GATEWAY_OUT) ./cmd/server
	@ls -l $(GATEWAY_OUT)

## apk: build a signed release APK.
apk: gateway
	$(GRADLE) assembleRelease --no-daemon
	@echo "APK: app/build/outputs/apk/release/app-release.apk"

## test: run every Go test suite.
test:
	cd $(GO_SRC) && $(GO) test ./...

## vet: static analysis across the module.
vet:
	cd $(GO_SRC) && $(GO) vet ./...

## fmt: report files that are not gofmt-clean.
fmt:
	@cd $(GO_SRC) && files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then echo "not formatted:"; echo "$$files"; exit 1; fi; \
	echo "gofmt clean"

## version: print the version that would be stamped into the build.
version:
	@echo $(VERSION)

## clean: remove build outputs. The tracked gateway binary is left alone so a
## clean checkout still builds; use `make clean-all` to drop it too.
clean:
	$(GRADLE) clean --no-daemon || true

clean-all: clean
	rm -f $(GATEWAY_OUT)
