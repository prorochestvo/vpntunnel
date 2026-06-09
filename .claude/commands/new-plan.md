---
description: Create a new plan file in plans/ with the documented template
argument-hint: <slug-in-kebab-case>
---

Create a new plan file in `plans/` for the slug `$ARGUMENTS`.

Steps:

1. Validate that `$ARGUMENTS` is non-empty and kebab-case (lowercase letters, digits, and hyphens). If not, stop and ask the user for a valid slug.
2. Determine the next plan number `NNN`:
   - List `plans/*.md`, `plans/completed/*.md`, `plans/history/*.md`.
   - Use only the `NNN-*.md` prefix in `plans/` and `plans/history/` to compute the next number. Increment by 1 and zero-pad to 3 digits.
3. Write `plans/NNN-$ARGUMENTS.md` with this template, replacing `<...>` placeholders:

```markdown
# Task Breakdown

## Overview

<one-paragraph description of the task and its motivation>

## Assumptions

- <assumption 1>
- <assumption 2>

## Tasks

### Task 1: <Title>
- Description: <what needs to be done>
- Acceptance Criteria:
  - <criterion 1>
  - <criterion 2>
- Pitfalls & edge cases: <list>
- Complexity: Easy | Medium | Hard

### Task 2: <Title>
- ...

## Execution Order

1. Task 1
2. Task 2
3. ...

## Risks

- <risk 1>
- <risk 2>

## Trade-offs

- <trade-off 1>
```

4. Report the created path and the chosen number.

Do not write any production code. This command only creates the plan file.
