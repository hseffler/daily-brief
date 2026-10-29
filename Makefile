# Makefile for Daily Brief

# Build variables
BINARY_NAME := daily-brief
VERSION := 1.3.0
BUILD_DIR := build
EXAMPLE_CONFIG_FILE := daily-brief.yaml.example

# Target OS/arch for the build. Overridable so the binary can be built for the
# host (e.g. `make build GOOS= GOARCH=`) or cross-compiled. Defaults to a Linux
# amd64 binary.
GOOS ?= linux
GOARCH ?= amd64

# Install variables
INSTALL_DIR := /opt/daily-brief
CONFIG_DIR := /opt/daily-brief
SYSTEMD_DIR := /etc/systemd/system
SERVICE_FILE := daily-brief.service
TIMER_FILE := daily-brief.timer

# Build the binary
build:
	@echo "Building $(BINARY_NAME) version $(VERSION) (GOOS=$(GOOS) GOARCH=$(GOARCH))..."
	@mkdir -p $(BUILD_DIR)
	@GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf $(BUILD_DIR)
	@echo "Clean complete"

# Run the application once (for testing) with the real config
run:
	@echo "Running $(BINARY_NAME)..."
	@./$(BUILD_DIR)/$(BINARY_NAME) -config daily-brief.yaml

# Test the build
test:
	@echo "Testing build..."
	@go test -v ./...
	@echo "Tests complete"

# Install the service. Requires root (writes to /opt and /etc/systemd).
install: build
	@echo "Installing $(BINARY_NAME) service..."
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "Error: 'make install' requires root (writes to $(INSTALL_DIR) and $(SYSTEMD_DIR)). Re-run with sudo." >&2; \
		exit 1; \
	fi
	@# Create the dedicated service user/group if missing (idempotent). The
	@# unit runs as this user; without it systemd fails with exit 217/USER.
	@if ! id $(BINARY_NAME) >/dev/null 2>&1; then \
		echo "Creating system user '$(BINARY_NAME)'..."; \
		useradd --system --no-create-home --shell /usr/sbin/nologin $(BINARY_NAME); \
	fi
	@mkdir -p $(CONFIG_DIR)
	cp $(BUILD_DIR)/$(BINARY_NAME) $(INSTALL_DIR)/$(BINARY_NAME)
	cp $(BINARY_NAME).service $(SYSTEMD_DIR)/$(SERVICE_FILE)
	cp $(BINARY_NAME).timer $(SYSTEMD_DIR)/$(TIMER_FILE)
	test -f $(CONFIG_DIR)/daily-brief.yaml || cp $(EXAMPLE_CONFIG_FILE) $(CONFIG_DIR)/daily-brief.yaml
	@# The binary and the install directory stay root-owned (the service only
	@# reads them); the config carries secrets, so it belongs to the service
	@# user with mode 600 (not world-readable).
	chmod 755 $(INSTALL_DIR)/$(BINARY_NAME)
	chown $(BINARY_NAME):$(BINARY_NAME) $(CONFIG_DIR)/daily-brief.yaml
	chmod 600 $(CONFIG_DIR)/daily-brief.yaml
	systemctl daemon-reload
	systemctl enable $(BINARY_NAME).timer
	systemctl start $(BINARY_NAME).timer
	@echo "Installation complete. Timer started (daily 05:00)."
	@echo "Edit $(CONFIG_DIR)/daily-brief.yaml with your API keys before the first run!"

# Uninstall the service. Requires root. The rm -rf is guarded: CONFIG_DIR must
# resolve under /opt/daily-brief or the command aborts rather than risk wiping
# an unrelated path if the variable were ever misconfigured.
uninstall:
	@echo "Uninstalling $(BINARY_NAME) service..."
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "Error: 'make uninstall' requires root. Re-run with sudo." >&2; \
		exit 1; \
	fi
	@realconfig="$$(readlink -f $(CONFIG_DIR))"; \
	if [ "$${realconfig}" != "/opt/daily-brief" ]; then \
		echo "Error: refusing to rm -rf CONFIG_DIR '$${realconfig}' (expected /opt/daily-brief). Aborting." >&2; \
		exit 1; \
	fi
	systemctl stop $(BINARY_NAME).timer 2>/dev/null || true
	systemctl disable $(BINARY_NAME).timer 2>/dev/null || true
	systemctl stop $(BINARY_NAME).service 2>/dev/null || true
	systemctl disable $(BINARY_NAME).service 2>/dev/null || true
	rm -f $(SYSTEMD_DIR)/$(SERVICE_FILE) $(SYSTEMD_DIR)/$(TIMER_FILE)
	rm -f $(INSTALL_DIR)/$(BINARY_NAME)
	rm -rf $(CONFIG_DIR)
	systemctl daemon-reload
	@# Remove the dedicated service user if it still exists. userdel without
	@# --remove refuses to touch a user with running processes, which is what we
	@# want: a leftover process means something else may still own its files.
	@if id $(BINARY_NAME) >/dev/null 2>&1; then \
		echo "Removing system user '$(BINARY_NAME)'..."; \
		userdel $(BINARY_NAME) || echo "Warning: could not remove user '$(BINARY_NAME)' (in use?). Left in place."; \
	fi
	@echo "Uninstallation complete."

# Show help
help:
	@echo "Makefile for Daily Brief"
	@echo ""
	@echo "Usage:"
	@echo "  make build          - Build the binary (GOOS=$(GOOS) GOARCH=$(GOARCH); override e.g. GOOS= GOARCH=)"
	@echo "  make clean          - Clean build artifacts"
	@echo "  make run            - Run the application once (for testing)"
	@echo "  make test           - Run tests"
	@echo "  make install        - Install systemd service + timer (requires root)"
	@echo "  make uninstall      - Uninstall systemd service + timer (requires root)"
	@echo "  make help           - Show this help message"

.PHONY: build clean run test help install uninstall
