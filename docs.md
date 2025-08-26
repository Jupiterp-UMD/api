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
| `courseCodes` (optional) | A string of one or multiple comma-separated course codes to fetch course data for; cannot set both `courseCodes` and `prefix`. | `courseCodes=CMSC132,MATH141` |
| `prefix` (optional) | The course prefix to match records to; for instance, `CMSC1` would match all CMSC1XX courses (like CMSC131 and CMSC132); cannot set both `courseCodes` and `prefix`. | `prefix=CMSC1` |
| `genEds` (optional) | A string of one or multiple comma-separated Gen-Eds to filter for; if multiple Gen-Eds are included, the API will return courses that satisfy all listed Gen-Eds. | `genEds=DVUP,DSSP` |
| `credits` (optional) | A string of equalities/inequalities to filter courses by how many credits they have. For courses with a range of possible credit values, filters by the minimum number of credits. Possible equality/inequality expressions are: `eq`, `lte`, `lt`, `gt`, `gte`, `neq` (for equal to, less than or equal to, less than, etc.). For multiple conditions, use multiple `credits` arguments. | `credits=gt.1&credits=lt.5` |
|`limit` (optional) | Maximum number of course records to return; defaults to 100, maximum of 500. | `limit=10` |
| `offset` (optional) | How many records to skip when returning courses; defaults to 0 | `offset=10` |
| `sortBy` (optional) | A comma-separated list of which columns to sort by when returning; can be sorted in ascending (`.asc`) or descending (`.desc`) order. | `sortBy=name.asc,min_credits.desc` |

#### Examples

##### Getting multiple specific courses

Request: `GET http://api.jupiterp.com/v0/courses?courseCodes=CMSC131,MATH141`

Response:
```
[
  {
    "course_code": "CMSC131",
    "name": "Object-Oriented Programming I",
    "min_credits": 4,
    "max_credits": null,
    "gen_eds": null,
    "conditions": [
      "Corequisite: MATH140. ",
      "Credit only granted for: CMSC131, CMSC133 or CMSC141."
    ],
    "description": "Introduction to programming and computer science. Emphasizes understanding and implementation of applications using object-oriented techniques. Develops skills such as program design and testing as well as implementation of programs using a graphical IDE. Programming done in Java."
  },
  {
    "course_code": "MATH141",
    "name": "Calculus II",
    "min_credits": 4,
    "max_credits": null,
    "gen_eds": null,
    "conditions": [
      "Prerequisite: Minimum grade of C- in MATH140."
    ],
    "description": "Continuation of MATH140, including techniques of integration, improper integrals, applications of integration (such as volumes, work, arc length, moments), inverse functions, exponential and logarithmic functions, sequences and series."
  }
]
```

##### Getting courses that satisfy Gen-Ed requirements

Request: `GET http://api.jupiterp.com/v0/courses?genEds=DVUP,DSSP&limit=2&sortBy=courseCode.asc`

Response:
```
[
  {
    "course_code": "AAST351",
    "name": "Asian Americans and Media",
    "min_credits": 3,
    "max_credits": null,
    "gen_eds": [
      "DSSP",
      "DVUP"
    ],
    "conditions": [
      "Credit only granted for: AAST351, AAST398M or AAST398N. ",
      "Formerly: AAST398M, AAST398N."
    ],
    "description": "From yellow peril invaders to model minority allies, Asian Americans have crafted their own dynamic cultural expressions in a number of media from film, television, and music to fashion, sports, and food that reveal and contest the contradictions of the U.S. nation-state. Asian American culture also uniquely sits at the nexus of immigration flows and digital technologies, providing a transnational lens to view the US place in the world. This advanced course, then, will introduce students to the study and practice of Asian American culture as multiple , hybrid, and heterogeneous. It will do so through three sections: section one will introduce students to classical, cultural, and media concepts as well as relevant keywords outlined by Asian American Studies scholars; section two will review the work of Asian American cultural theorists; section three will focus on analyses of particular Asian American cultural productions. In doing so, students will gain an understanding of the shifting and interlocking tensions among the local, the national, and the global that form the cultural geographies of Asian America."
  },
  {
    "course_code": "AMST320",
    "name": "(Dis)ability in American Film",
    "min_credits": 3,
    "max_credits": null,
    "gen_eds": [
      "DSHU",
      "DSSP",
      "DVUP"
    ],
    "conditions": [
      "Credit only granted for: AMST320 or AMST328X. ",
      "Formerly: AMST328X."
    ],
    "description": "Explores the connection between film and disability through an analysis of independent and mainstream American films in various film genres. Specifically, we will consider how these film representations reflect and/or challenge the shifting social perspectives of disability over the 20th and 21st centuries.  Beginning with the presentation of disability as theatrical spectacle in the traveling sideshow and early cinema, we will work our way through film history to develop an understanding of our society's complicated relationship with disability."
  }
]
```