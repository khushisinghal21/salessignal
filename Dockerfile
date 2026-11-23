FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /salesignal ./cmd/salesignal

FROM gcr.io/distroless/static
COPY --from=build /salesignal /salesignal
ENV ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/salesignal"]
