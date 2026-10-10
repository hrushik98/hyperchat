# Harper

## Register

product

## Users

A single user chatting locally with DeepSeek models, as described in AGENTS.md and README.md.

## Product Purpose

A Vue and Go chat app with streamed replies, model selection, thinking mode, and saved conversations. The frontend runs on port 4003 and the backend runs on port 4004 through Docker Compose.

## Design Reference

The user supplied a chat screenshot and explicitly requested that the UI match it. The conversation is the primary surface; navigation is secondary.

## Design Principles

- Keep prompts, replies, and composing easy to scan.
- Preserve existing conversation and model controls.
- Use the supplied reference for the visual direction.

## Accessibility & Inclusion

Preserve the existing keyboard shortcuts, accessible control names, focus indicators, and reduced-motion support. No additional user-specific requirements have been stated.
