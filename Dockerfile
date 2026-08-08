# The serve container for compose stacks and spawned-worker topologies.
# Pure-Go SQLite, so the final image is the static binary and nothing else.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /loopflow ./cmd/loopflow

FROM scratch
COPY --from=build /loopflow /loopflow
ENV LOOPFLOW_ROOT=/state
VOLUME /state
EXPOSE 7171
# The token comes from LOOPFLOW_TOKEN in the environment. Without one, serve
# generates and prints a fresh token each start — fine at a terminal, useless
# in compose, so set it.
ENTRYPOINT ["/loopflow", "serve", "-listen", ":7171"]
