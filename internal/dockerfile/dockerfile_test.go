package dockerfile

import (
	"reflect"
	"strings"
	"testing"
)

func TestBaseImagesMultiStage(t *testing.T) {
	df := `
# syntax=docker/dockerfile:1
ARG GO_VERSION=1.22
ARG BASE=alpine:3.19
ARG UNSET_ARG

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
RUN go build \
    -o /out/app \
    ./...

FROM ${BASE} as runtime
COPY --from=build /out/app /app

FROM scratch
COPY --from=runtime /app /app

FROM build AS test
RUN go test ./...

FROM 0
RUN echo "reference by index"

FROM ${UNSET_ARG:-busybox:stable}
FROM nginx:${UNSET_ARG}
`
	got, err := BaseImages(strings.NewReader(df), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"golang:1.22-alpine", "alpine:3.19", "busybox:stable", "nginx:"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestBuildArgsOverride(t *testing.T) {
	df := "ARG TAG=latest\nFROM nginx:$TAG\n"
	got, err := BaseImages(strings.NewReader(df), map[string]string{"TAG": "1.25"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"nginx:1.25"}) {
		t.Fatalf("got %v", got)
	}
}

func TestArgAfterFromIgnored(t *testing.T) {
	df := "FROM nginx\nARG X=1\nFROM redis:$X\n"
	got, _ := BaseImages(strings.NewReader(df), nil)
	if !reflect.DeepEqual(got, []string{"nginx", "redis:"}) {
		t.Fatalf("got %v", got)
	}
}

func TestEscapeDirective(t *testing.T) {
	df := "# escape=`\nFROM `\n  nginx:1\n"
	got, _ := BaseImages(strings.NewReader(df), nil)
	if !reflect.DeepEqual(got, []string{"nginx:1"}) {
		t.Fatalf("got %v", got)
	}
}

func TestDedup(t *testing.T) {
	df := "FROM alpine AS a\nFROM alpine AS b\nFROM a\n"
	got, _ := BaseImages(strings.NewReader(df), nil)
	if !reflect.DeepEqual(got, []string{"alpine"}) {
		t.Fatalf("got %v", got)
	}
}

func TestStages(t *testing.T) {
	st, err := Parse(strings.NewReader("FROM --platform=linux/amd64 a:1 AS one\nFROM one\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 || st[0].Platform != "linux/amd64" || st[0].Name != "one" || !st[1].FromStage {
		t.Fatalf("unexpected stages %+v", st)
	}
}
