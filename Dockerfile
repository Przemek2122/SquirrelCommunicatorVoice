# Stage 1: Build the application
FROM golang:1.25-alpine AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy dependency files and download them
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the executable
# CGO_ENABLED=0 ensures a statically linked binary (no external C libraries required)
RUN CGO_ENABLED=0 GOOS=linux go build -o voice_app .

# Stage 2: Lightweight target image
FROM alpine:latest

# Add CURL for health check on docker
RUN apk add --no-cache curl

# Set the working directory for the final image
WORKDIR /root/

# Copy the compiled binary from the builder stage
COPY --from=builder /app/voice_app .

# Expose the port the Go application runs on (change if necessary)
EXPOSE 8082

# Run the application
CMD ["./voice_app"]