APP_NAME=proxy
BIN_DIR=bin
GO_VERSION=1.25

# Default build (CLI version)
build:
	@echo ">> Building $(APP_NAME) CLI..."
	@mkdir -p $(BIN_DIR)
	@go build -o $(BIN_DIR)/$(APP_NAME) main.go app.go

# Build GUI version for macOS
build-gui:
	@echo ">> Building $(APP_NAME) GUI..."
	@mkdir -p $(BIN_DIR)
	@go build -o $(BIN_DIR)/$(APP_NAME)-gui main.go app.go

# Run CLI version
run: build
	@echo ">> Running $(APP_NAME) CLI..."
	@./$(BIN_DIR)/$(APP_NAME) proxies.json :8080

# Run GUI version
run-gui: build-gui
	@echo ">> Running $(APP_NAME) GUI..."
	@./$(BIN_DIR)/$(APP_NAME)-gui --gui

clean:
	@echo ">> Cleaning..."
	@rm -rf $(BIN_DIR)

docker-build:
	@echo ">> Building Docker image..."
	@docker build -t $(APP_NAME):latest .

docker-run:
	@echo ">> Running Docker container..."
	@docker run --rm -p 8080:8080 -v $(PWD)/proxies.json:/app/proxies.json $(APP_NAME):latest
