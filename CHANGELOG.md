# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]
### Added
- Initial release: OpenAI-compatible reverse proxy with stream duplication detection (min_count/min_len/max_len/max_gap), env + config.yaml configuration (env wins), Docker deployment (distroless), GitHub Actions CI (tests.yml) and release workflow pushing to ghcr.io.
- Prometheus observability at `GET /metrics`: seven `anti_loop_*` metrics (requests, stream requests, stream cuts, request duration, active streams, upstream errors, cut span length) labeled by the request's `model`.
