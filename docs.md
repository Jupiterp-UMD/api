# Jupiterp API v0 Docs (pre-release)

## Introduction

Welcome to the Jupiterp API, a free and open-source API to get detailed course data for the University of Maryland. Currently, the API is in pre-release phase and is unstable; expect breaking changes, but the information in these docs should be correct and up-to-date.

For any questions or bugs, please contact [admin@jupiterp.com](mailto:admin@jupiterp.com).

Feel free to view or contribute to the project [on GitHub](https://www.github.com/jupiterp-umd/api).

## Endpoints

| path | description | link |
| :-- | :-- | :-- |
| `/v0/` | Base endpoint | [jump](#-v0-) |
| `/v0/courses` | Get a list of courses with full course info | [jump](#-v0-courses-) |
| `/v0/courses/minified` | Get a list of courses with just the code and title for each | [jump](#-v0-courses-minified-) |
| `/v0/courses/withSections` | Get a list of courses, including section data for each course | [jump](#-v0-courses-withsections-) |
| `/v0/sections` | Get a list of sections for courses | [jump](#-v0-sections-) |

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

#### Output

| field | type | description |
| :-- | :--: | :-- |
| `course_code` | string | The unique course code for a course, which consists of a four-letter department code, a three digit course identifier, and optional letters at the end. |
| `name` | string | The name of a course, providing very brief information about the subject of the course. |
| `min_credits` | int | The minimum number of credits this course is worth. For courses that do not have a range of possible credit values, this field is simply the number of credits the course is worth. All courses have a non-null `min_credits` field. |
| `max_credits` | int or null | The maximum number of credits a course is worth, if the course has a range of possible credit values. For courses that have a set credit value, this field will be null. |
| `gen_eds` | string[] or null | A list of four-letter codes for the Gen-Ed requirements this course satisfies (ex. DSSP, DVUP). |
| `conditions` | string[] or null | A list of additionall conditions listed for this course. This consists of things like prerequisites, corequisites, or additional information. |
| `description` | string or null | A detailed description of the course. Some courses do not have a description, especially independent research courses. |

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
    "description": "Introduction to programming and computer science.
    Emphasizes understanding and implementation of applications using
    object-oriented techniques. Develops skills such as program design and
    testing as well as implementation of programs using a graphical IDE.
    Programming done in Java."
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
    "description": "Continuation of MATH140, including techniques of
    integration, improper integrals, applications of integration (such as
    volumes, work, arc length, moments), inverse functions, exponential and
    logarithmic functions, sequences and series."
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
    "description": "From yellow peril invaders to model minority allies, Asian 
    Americans have crafted their own dynamic cultural expressions in a number 
    of media from film, television, and music to fashion, sports, and food that
    reveal and contest the contradictions of the U.S. nation-state. Asian
    American culture also uniquely sits at the nexus of immigration flows and
    digital technologies, providing a transnational lens to view the US place
    in the world. This advanced course, then, will introduce students to the
    study and practice of Asian American culture as multiple , hybrid, and
    heterogeneous. It will do so through three sections: section one will
    introduce students to classical, cultural, and media concepts as well as
    relevant keywords outlined by Asian American Studies scholars; section two
    will review the work of Asian American cultural theorists; section three
    will focus on analyses of particular Asian American cultural productions.
    In doing so, students will gain an understanding of the shifting and
    interlocking tensions among the local, the national, and the global that
    form the cultural geographies of Asian America."
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
    "description": "Explores the connection between film and disability
    through an analysis of independent and mainstream American films in various
    film genres. Specifically, we will consider how these film representations
    reflect and/or challenge the shifting social perspectives of disability
    over the 20th and 21st centuries.  Beginning with the presentation of
    disability as theatrical spectacle in the traveling sideshow and early
    cinema, we will work our way through film history to develop an
    understanding of our society's complicated relationship with disability."
  }
]
```

### `/v0/courses/minified`

Gets a minified list of courses that satisfy the given parameters. Takes the same parameters as the `/v0/courses` endpoint, but returns only the course code and title.

#### Query parameters

Same as the parameters for `/v0/courses`; see [here](#-v0-courses-).

#### Output

| field | type | description |
| :-- | :--: | :-- |
| `course_code` | string | The unique course code for a course, which consists of a four-letter department code, a three digit course identifier, and optional letters at the end. |
| `name` | string | The name of a course, providing very brief information about the subject of the course. |

#### Examples

##### Getting courses with a specific prefix

Request: `GET http://api.jupiterp.com/v0/courses/minified?prefix=ASTR4&sortBy=name.asc`

Response:
```
[
  {
    "course_code": "ASTR422",
    "name": "Cosmology"
  },
  {
    "course_code": "ASTR421",
    "name": "Galaxies"
  },
  {
    "course_code": "ASTR498",
    "name": "Special Problems in Astronomy"
  }
]
```

### `/v0/courses/withSections`

Gets a list of full courses data and associated sections data. Each returned course also contains a (potentially-empty) list of sections for that course.

#### Query parameters

Same as the parameters for `/v0/courses`; see [here](#-v0-courses-).

#### Output

| field | type | description |
| :-- | :--: | :-- |
| `course_code` | string | The unique course code for a course, which consists of a four-letter department code, a three digit course identifier, and optional letters at the end. |
| `name` | string | The name of a course, providing very brief information about the subject of the course. |
| `min_credits` | int | The minimum number of credits this course is worth. For courses that do not have a range of possible credit values, this field is simply the number of credits the course is worth. All courses have a non-null `min_credits` field. |
| `max_credits` | int or null | The maximum number of credits a course is worth, if the course has a range of possible credit values. For courses that have a set credit value, this field will be null. |
| `gen_eds` | string[] or null | A list of four-letter codes for the Gen-Ed requirements this course satisfies (ex. DSSP, DVUP). |
| `conditions` | string[] or null | A list of additionall conditions listed for this course. This consists of things like prerequisites, corequisites, or additional information. |
| `description` | string or null | A detailed description of the course. Some courses do not have a description, especially independent research courses. |
| `sections` | Section[] | A list of `Section`s. A `Section` consists of the fields described in the output of `/v0/sections` (see [here](#-v0-sections-)) |

#### Examples

##### Getting a course with sections data

Request: `GET http://api.jupiterp.com/v0/courses/withSections?courseCodes=CMSC433`

Response:
```
[
  {
    "course_code": "CMSC433",
    "name": "Programming Language Technologies and Paradigms",
    "min_credits": 3,
    "max_credits": null,
    "gen_eds": null,
    "conditions": [
      "Prerequisite: Minimum grade of C- in CMSC330; or must be in the
      (Computer Science (Doctoral), Computer Science (Master's)) program. ",
      "Restriction: Permission of CMNS-Computer Science department."
    ],
    "description": "Programming language technologies (e.g., object-oriented
    programming), their implementations and use in software design and
    implementation.",
    "sections": [
      {
        "holdfile": 0,
        "meetings": [
          "TuTh-11:00am-12:15pm-CSI-1115"
        ],
        "sec_code": "0101",
        "waitlist": 3,
        "open_seats": 0,
        "course_code": "CMSC433",
        "instructors": [
          "Anwar Mamat"
        ],
        "total_seats": 140
      },
      {
        "holdfile": null,
        "meetings": [
          "TuTh-3:30pm-4:45pm-IRB-0318"
        ],
        "sec_code": "0201",
        "waitlist": 0,
        "open_seats": 7,
        "course_code": "CMSC433",
        "instructors": [
          "Anwar Mamat"
        ],
        "total_seats": 50
      }
    ]
  }
]
```

### `/v0/sections`

Get sections for specific courses, or for all courses that match a course code prefix. Note that some courses don't have any sections; for example, most independent research courses, like ASTR498, will not return any sections.

#### Query parameters

| param | description | example |
|:--|:--|:--|
| `courseCodes` (optional) | A string of one or multiple comma-separated course codes to fetch course data for; cannot set both `courseCodes` and `prefix`. | `courseCodes=CMSC132,MATH141` |
| `prefix` (optional) | The course prefix to match records to; for instance, `CMSC1` would match all CMSC1XX courses (like CMSC131 and CMSC132); cannot set both `courseCodes` and `prefix`. | `prefix=CMSC1` |
|`limit` (optional) | Maximum number of course records to return; defaults to 100, maximum of 500. | `limit=10` |
| `offset` (optional) | How many records to skip when returning courses; defaults to 0 | `offset=10` |
| `sortBy` (optional) | A comma-separated list of which columns to sort by when returning; can be sorted in ascending (`.asc`) or descending (`.desc`) order. | `sortBy=name.asc,min_credits.desc` |

#### Output

| field | type | description |
| :-- | :--: | :-- |
| `course_code` | string | The unique course code for a course, which consists of a four-letter department code, a three digit course identifier, and optional letters at the end. |
| `sec_code` | string | The code (usually 4 numbers, but sometimes includes letters) for a section. The `sec_code` is unique within a given course, but a section may have the same `sec_code` as a section with a different `course_code`. |
| `instructors` | string[] | A list of the names of instructors teaching a course. Instructor may not be an actual name; for instance, it could be "Instructor: TBA". |
| `meetings` | string[] | A list of meeting times and places for the section. Meeting strings have different formats depending on the class format: <ul><li>In-person, synchronous: "Days-StartTime-EndTime-Building-Room"</li><li>Online, synchronous: "Days-StartTime-EndTime-OnlineSync"</li><li>Online, asynchronous: "OnlineAsync"</li><li>Unspecified: "Unspecified"</li></ul> Some courses can have both synchronous and asynchronous meetings. |
|`open_seats` | int | The number of available seats for this section. |
| `total_seats` | int | The total number of seats in this section. |
| `waitlist` | int | How many people are on the waitlist for this section. |
| `holdfile` | int or null | The number of people on the holdfile for this section, if a holdfile exists. |

#### Examples

##### Getting all sections for a course

Request: `GET http://api.jupiterp.com/v0/sections?courseCodes=CMSC433`

Response:
```
[
  {
    "course_code": "CMSC433",
    "sec_code": "0101",
    "instructors": [
      "Anwar Mamat"
    ],
    "meetings": [
      "TuTh-11:00am-12:15pm-CSI-1115"
    ],
    "open_seats": 0,
    "total_seats": 140,
    "waitlist": 7,
    "holdfile": 0
  },
  {
    "course_code": "CMSC433",
    "sec_code": "0201",
    "instructors": [
      "Anwar Mamat"
    ],
    "meetings": [
      "TuTh-3:30pm-4:45pm-IRB-0318"
    ],
    "open_seats": 15,
    "total_seats": 50,
    "waitlist": 0,
    "holdfile": null
  }
]
```