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

# Copy the Firebase service account JSON
COPY prueba-perros-23231-firebase-adminsdk-fbsvc-0558e06804.json .

# Expose the port (Render will use $PORT)
EXPOSE 8080

# Run the application
CMD ["./main"]
