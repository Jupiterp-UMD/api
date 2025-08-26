# Jupiterp API v0 Docs (pre-release)

## Introduction

Welcome to the Jupiterp API, a free and open-source API to get detailed course data for the University of Maryland. Currently, the API is in pre-release phase and is unstable; expect breaking changes, but the information in these docs should be correct and up-to-date.

For any questions or bugs, please contact [admin@jupiterp.com](mailto:admin@jupiterp.com).

Feel free to view or contribute to the project [on GitHub](https://www.github.com/jupiterp-umd/api).

## Endpoints

### `/v0/`

This is the base endpoint for v0 of the Jupiterp API. It will simply return a HTTP StatusOK with some text to indicate that the Jupiterp API is online.

### `/v0/courses`

Gets a list of courses that match the given query parameters. This endpoint does not return section information; for section info, use the `sections` endpoint listed below.

#### Query parameters

| param | description | example |
|:--|:--|:--|
| `courseCodes` | A string of one or multiple comma-separated course codes to fetch course data for; cannot set both `courseCodes` and `prefix`, but one must be set. | `courseCodes=CMSC132,MATH141` |
| `prefix` | The course prefix to match records to; for instance, `CMSC1` would match all CMSC1XX courses (like CMSC131 and CMSC132). | `prefix=CMSC1` |
| `genEds` (optional) | A string of one or multiple comma-separated Gen-Eds to filter for; if multiple Gen-Eds are included, the API will return courses that satisfy all listed Gen-Eds. | `genEds=DVUP,DSSP` |
| `credits` (optional) | A string of equalities/inequalities to filter courses by how many credits they have. For courses with a range of possible credit values, filters by the minimum number of credits. Possible equality/inequality expressions are: `eq`, `lte`, `lt`, `gt`, `gte`, `neq` (for equal to, less than or equal to, less than, etc.). For multiple conditions, use multiple `credits` arguments. | `credits=gt.1&credits=lt.5` |
|`limit` (optional) | Maximum number of course records to return; defaults to 100, maximum of 500. | `limit=10` |
| `offset` (optional) | How many records to skip when returning courses; defaults to 0 | `offset=10` |
| `sortBy` (optional) | A comma-separated list of which columns to sort by when returning. | `sortBy=name,min_credits` |