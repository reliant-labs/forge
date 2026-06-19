FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
COPY .forge-pkg/ ./.forge-pkg/
RUN go mod download
COPY . .
RUN go build -o /app/bin/app ./cmd
