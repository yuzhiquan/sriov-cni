BINARY := build/sriov
VERSION ?= v0.1.0
CNI_BIN_DIR ?= /opt/cni/bin

export GOPROXY ?= https://goproxy.cn,direct
export CGO_ENABLED := 0

.PHONY: all build test vet fmt clean install smoke

all: fmt vet test build

build:
	@mkdir -p build
	go build -trimpath -ldflags "-s -w" -o $(BINARY) ./cmd/sriov
	@echo "built: $(BINARY)"

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf build

install: build
	install -m 0755 $(BINARY) $(CNI_BIN_DIR)/sriov
	@echo "installed: $(CNI_BIN_DIR)/sriov"

# 需要有真实 SR-IOV 硬件
smoke: build
	@echo "==> 检查硬件"
	sudo ./deploy/setup-vfs.sh --check
	@echo "==> 手动调用插件"
	sudo ./deploy/test-cni.sh --plugin $(CURDIR)/$(BINARY)
