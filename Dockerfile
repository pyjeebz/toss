# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# CRITICAL -- SCALE THIS TO EXACTLY ONE REPLICA.
#
# Every room, item and subscriber lives in this process's memory. There is no
# database and no cross-process fan-out. Run two of these behind a load
# balancer and a POST landing on container A while the recipient's SSE stream
# is held by container B is delivered to nobody: no error, no log line, a 201
# to the sender. It presents as "toss is flaky", not as a misconfiguration.
#
#   docker run -p 8080:8080 toss     good
#   replicas: 2                      silently broken
#   autoscaling                      silently broken
#
# Horizontal scale is a real change -- a shared bus (Redis pub/sub, NATS) or
# sticky routing keyed on room ID -- not a replica count.
# ---------------------------------------------------------------------------

FROM golang:1.23-alpine AS build
WORKDIR /src

# Dependencies first, so edits to the source do not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static, stripped, reproducible. The frontend rides along via embed.FS, so
# this binary is the entire artifact.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /toss ./cmd/toss

FROM gcr.io/distroless/static:nonroot
COPY --from=build /toss /toss
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/toss"]
