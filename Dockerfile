# Use official Go image as a builder
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Copy go.mod and go.sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Build the application
RUN go build -o main .

# Use a small alpine image for the final container
FROM alpine:latest

WORKDIR /app

# Copy the binary from the builder
COPY --from=builder /app/main .

# Note: The Firebase JSON is not copied here. 
# It is provided as an environment variable (FIREBASE_CONFIG) in Render.

# Expose the port (Render will use $PORT)
EXPOSE 8080

# Run the application
CMD ["./main"]
