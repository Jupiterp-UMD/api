# Jupiterp API Docs (pre-release)

## Introduction

Welcome to the Jupiterp API, a free and open-source API to get detailed course data for the University of Maryland. Currently, the API is in pre-release phase and is unstable; expect breaking changes, but the information in these docs should be correct and up-to-date.

For any questions or bugs, please contact [admin@jupiterp.com](mailto:admin@jupiterp.com).

Feel free to view or contribute to the project [on GitHub](https://www.github.com/jupiterp-umd/api).

## Versions

Everything is served under **`/v1`**. Use it for new work.

**`/v0` still works and is not going away.** Every read endpoint documented here
answers on both prefixes, from the same handlers, returning the same bytes —
`/v0/courses` and `/v1/courses` are the same endpoint. Existing clients,
including [`@jupiterp/jupiterp`](https://www.npmjs.com/package/@jupiterp/jupiterp)
1.x, need no change and there is no migration deadline.

The two prefixes are held together by a parity check that compares every read
endpoint across both, so they cannot quietly drift apart.

## Endpoints

| path | description | link |
| :-- | :-- | :-- |
| `/v1/` | Base endpoint | [jump](#-v1-) |
| `/v1/courses` | Get a list of courses with full course info | [jump](#-v1-courses-) |
| `/v1/courses/minified` | Get a list of courses with just the code and title for each | [jump](#-v1-courses-minified-) |
| `/v1/courses/withSections` | Get a list of courses, including section data for each course | [jump](#-v1-courses-withsections-) |
| `/v1/sections` | Get a list of sections for courses | [jump](#-v1-sections-) |
| `/v1/instructors` | Get a list of instructors and their ratings | [jump](#-v1-instructors-) |
| `/v1/instructors/active` | Get a list of instructors actively teaching a course | [jump](#-v1-instructors-active-) |
| `/v1/deptList` | Get a list of 4-letter department codes | [jump](#-v1-deptlist-) |
| `/v1/grades` | Get grade distributions for individual sections | [jump](#-v1-grades-) |
| `/v1/grades/summary` | Get grade distributions aggregated by course, term, or instructor | [jump](#-v1-grades-summary-) |
| `/v1/grades/terms` | Get the terms for which grade data is available | [jump](#-v1-grades-terms-) |

### `/v1/` 

[(back to endpoints)](#endpoints)

This is the base endpoint for the Jupiterp API. It will simply return a HTTP StatusOK with some text to indicate that the Jupiterp API is online.

### `/v1/courses` 

[(back to endpoints)](#endpoints)

Gets a list of courses that match the given query parameters. This endpoint does not return section information; for section info, use the `sections` endpoint listed below.

#### Query parameters

| param | description | example |
|:--|:--|:--|
| `courseCodes` (optional) | A string of one or multiple comma-separated course codes to fetch course data for; cannot set both `courseCodes` and `prefix`. | `courseCodes=CMSC132,MATH141` |
| `prefix` (optional) | The course prefix to match records to; for instance, `CMSC1` would match all CMSC1XX courses (like CMSC131 and CMSC132); cannot set both `courseCodes` and `prefix`. | `prefix=CMSC1` |
| `number` (optional) | The course number to search for across multiple departments; for instance, `433` would match to courses like AOSC433, AREC433, etc. | `number=433` |
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

Request: `GET http://api.jupiterp.com/v1/courses?courseCodes=CMSC131,MATH141`

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

Request: `GET http://api.jupiterp.com/v1/courses?genEds=DVUP,DSSP&limit=2&sortBy=courseCode.asc`

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

### `/v1/courses/minified` 

[(back to endpoints)](#endpoints)

Gets a minified list of courses that satisfy the given parameters. Takes the same parameters as the `/v1/courses` endpoint, but returns only the course code and title.

#### Query parameters

Same as the parameters for `/v1/courses`; see [here](#-v1-courses-).

#### Output

| field | type | description |
| :-- | :--: | :-- |
| `course_code` | string | The unique course code for a course, which consists of a four-letter department code, a three digit course identifier, and optional letters at the end. |
| `name` | string | The name of a course, providing very brief information about the subject of the course. |

#### Examples

##### Getting courses with a specific prefix

Request: `GET http://api.jupiterp.com/v1/courses/minified?prefix=ASTR4&sortBy=name.asc`

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

### `/v1/courses/withSections` 

[(back to endpoints)](#endpoints)

Gets a list of full courses data and associated sections data. Each returned course also contains a (potentially-empty) list of sections for that course.

#### Query parameters

| param | description | example |
|:--|:--|:--|
| `courseCodes` (optional) | A string of one or multiple comma-separated course codes to fetch course data for; cannot set both `courseCodes` and `prefix`. | `courseCodes=CMSC132,MATH141` |
| `prefix` (optional) | The course prefix to match records to; for instance, `CMSC1` would match all CMSC1XX courses (like CMSC131 and CMSC132); cannot set both `courseCodes` and `prefix`. | `prefix=CMSC1` |
| `number` (optional) | The course number to search for across multiple departments; for instance, `433` would match to courses like AOSC433, AREC433, etc. | `number=433` |
| `genEds` (optional) | A string of one or multiple comma-separated Gen-Eds to filter for; if multiple Gen-Eds are included, the API will return courses that satisfy all listed Gen-Eds. | `genEds=DVUP,DSSP` |
| `credits` (optional) | A string of equalities/inequalities to filter courses by how many credits they have. For courses with a range of possible credit values, filters by the minimum number of credits. Possible equality/inequality expressions are: `eq`, `lte`, `lt`, `gt`, `gte`, `neq` (for equal to, less than or equal to, less than, etc.). For multiple conditions, use multiple `credits` arguments. | `credits=gt.1&credits=lt.5` |
| `totalClassSize` (optional) | A string of equalities/inequalities to filter by the total number of seats in a section. Possible expressions are: `eq`, `lte`, `lt`, `gt`, `gte`, `neq` (for equal to, less than or equal to, less than, etc.). For multiple conditions, use multiple `totalClassSize` arguments. | `totalClassSize=gt.40&totalClassSize=le.50` |
| `onlyOpen` (optional) | If set to true, only returns sections with more than zero open seats. | `onlyOpen=true` |
| `instructor` (optional) | Return only sections that have the given instructor in the `instructors` field. This field is case-sensitive. | `instructor=Darryll%20Pines` |
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
| `sections` | Section[] | A list of `Section`s. A `Section` consists of the fields described in the output of `/v1/sections` (see [here](#-v1-sections-)) |

#### Examples

##### Getting a course with sections data

Request: `GET http://api.jupiterp.com/v1/courses/withSections?courseCodes=CMSC433`

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

### `/v1/sections` 

[(back to endpoints)](#endpoints)

Get sections for specific courses, or for all courses that match a course code prefix. Note that some courses don't have any sections; for example, most independent research courses, like ASTR498, will not return any sections.

#### Query parameters

| param | description | example |
|:--|:--|:--|
| `courseCodes` (optional) | A string of one or multiple comma-separated course codes to fetch course data for; cannot set both `courseCodes` and `prefix`. | `courseCodes=CMSC132,MATH141` |
| `prefix` (optional) | The course prefix to match records to; for instance, `CMSC1` would match all CMSC1XX courses (like CMSC131 and CMSC132); cannot set both `courseCodes` and `prefix`. | `prefix=CMSC1` |
| `totalClassSize` (optional) | A string of equalities/inequalities to filter by the total number of seats in a section. Possible expressions are: `eq`, `lte`, `lt`, `gt`, `gte`, `neq` (for equal to, less than or equal to, less than, etc.). For multiple conditions, use multiple `totalClassSize` arguments. | `totalClassSize=gt.40&totalClassSize=le.50` |
| `onlyOpen` (optional) | If set to true, only returns sections with more than zero open seats. | `onlyOpen=true` |
| `instructor` (optional) | Return only sections that have the given instructor in the `instructors` field. This field is case-sensitive. | `instructor=Darryll%20Pines` |
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

Request: `GET http://api.jupiterp.com/v1/sections?courseCodes=CMSC433`

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

### `/v1/instructors` 

[(back to endpoints)](#endpoints)

Get a list of all instructors and their average ratings, including instructors not actively teaching any courses.

#### Query parameters

| param | description | example |
|:--|:--|:--|
| `instructorNames` (optional) | A comma-separated list of instructor names to get results for. Cannot set both `instructorNames` and `instructorSlugs`. | `instructorNames=Testudo%20Terrapin,Darryll%20Pines` |
| `instructorSlugs` (optional) | A comma-separated list of instructor slugs to get results for; slugs are the internal identifier used to distinguish an instructor and are unique to each instructor. Cannot set both `instructorNames` and `instructorSlugs`. | `instructorSlugs=shane-walsh,darryll-pines` |
| `nameSearch` (optional) | Case-insensitive substring match on instructor name. Matched against a normalized form of the name, so accents and punctuation are ignored on both sides: `obrien` matches "O'Brien" and `jose` matches "José". | `nameSearch=walsh` |
| `activeOnly` (optional) | If true, only returns instructors currently teaching at least one section. | `activeOnly=true` |
| `count` (optional) | If true, the total number of matching records is returned in the `Content-Range` response header (`0-49/4812`). Costs an extra aggregate over the filtered set, so it is off by default. | `count=true` |
| `ratings` (optional) | A string of equalities/inequalities to filter instructors by their average rating on PlanetTerp. Possible equality/inequality expressions are: eq, lte, lt, gt, gte, neq (for equal to, less than or equal to, less than, etc.). For multiple conditions, use multiple ratings arguments. | `ratings=gt.3.14&ratings=lt.5` |
| `limit` (optional) | The number of results to return. Defaults to 100, maximum of 500. | `limit=10`|
|`offset` (optional) | How many records to skip when returning results; defaults to 0 | `offset=5` |
| `sortBy` (optional) | A comma-separated list of which columns to sort by when returning; can be sorted in ascending (`.asc`) or descending (`.desc`) order. | `sortBy=average_rating.asc,name.desc` |

#### Output

| field | type | description |
| :-- | :--: | :-- |
| `slug` | string | The internal string used to identify an individual instructor, unique to that instructor. See PlanetTerp API spec for more info. |
| `name` | string | The instructor's name as listed on PlanetTerp |
| `average_rating` | float | The average rating given to that professor from reviews on PlanetTerp |

#### Examples

##### Getting high-rated, non-5 star instructors

Request: `GET http://api.jupiterp.com/v1/instructors?ratings=gt.4.5&ratings=lt.5&limit=5&sortBy=average_rating.desc,name.desc`

Response:
```
[
  {
    "slug": "gramlich_meredith",
    "name": "Meredith Gramlich",
    "average_rating": 4.9667
  },
  {
    "slug": "cropper",
    "name": "Maureen Cropper",
    "average_rating": 4.9474
  },
  {
    "slug": "gruber_sean",
    "name": "Sean Gruber",
    "average_rating": 4.9398
  },
  {
    "slug": "o’brien",
    "name": "Terrence O’Brien",
    "average_rating": 4.9375
  },
  {
    "slug": "zomback",
    "name": "Jenna Zomback",
    "average_rating": 4.9355
  }
]
```

### `/v1/instructors/active`

[(back to endpoints)](#endpoints)

Get all instructors that are currently teaching a course, as listed on Testudo.

#### Query Parameters

Same as `/v1/instructors`; see [here](#-v1-instructors-).

#### Output

Same as `/v1/instructors`; see [here](#-v1-instructors-).

#### Examples

##### Getting instructors currently teaching a course

Request: `GET http://api.jupiterp.com/v1/instructors/active?limit=5`

Response:
```
[
  {
    "slug": "abadi_daniel",
    "name": "Daniel Abadi",
    "average_rating": 3.122
  },
  {
    "slug": "abasi",
    "name": "Ali Abasi",
    "average_rating": null
  },
  {
    "slug": "abbasi",
    "name": "Hossein Abbasi",
    "average_rating": 3.7791
  },
  {
    "slug": "abdul-alim",
    "name": "Jamaal Abdul-Alim",
    "average_rating": 3.25
  },
  {
    "slug": "abioye",
    "name": "Victor Abioye",
    "average_rating": null
  }
]
```

### `/v1/deptList`

[(back to endpoints)](#endpoints)

Get a list of 4-letter department codes.

#### Query parameters

None

#### Output

| field | type | description |
| :-- | :--: | :-- |
| `dept_code` | string | A unique 4-letter department code |
| `name` | string | The name of the department |
### `/v1/grades` 

[(back to endpoints)](#endpoints)

Gets grade distributions for individual course sections, as released by the University Registrar under the Maryland Public Information Act. Data covers fall and spring terms from Fall 2010 through Spring 2026; winter and summer terms were not released.

For counts aggregated across sections, terms, or instructors, use the `summary` endpoint below.

#### Query parameters

| param | description | example |
|:--|:--|:--|
| `courseCodes` (optional) | A string of one or multiple comma-separated course codes; cannot be combined with `prefix` or `number`. | `courseCodes=CMSC132,MATH141` |
| `prefix` (optional) | The course prefix to match records to; for instance, `CMSC1` would match all CMSC1XX courses. | `prefix=CMSC1` |
| `number` (optional) | The course number to search for across multiple departments. | `number=433` |
| `term` (optional) | A string of equalities/inequalities to filter by term code. Possible expressions are: `eq`, `lte`, `lt`, `gt`, `gte`, `neq`, and `in` for a specific set. For multiple conditions, use multiple `term` arguments. | `term=gte.202008` or `term=in.(202408,202501)` |
| `instructor` (optional) | Return only sections taught by the given instructor, written in "First Last" order. This field is case-sensitive. | `instructor=Larry%20Herman` |
| `instructorSource` (optional) | A comma-separated list of `instructor_source` values to include. Defaults to all. Use `reported,lead` to exclude attributions carried across lecture groups. | `instructorSource=reported` |
| `gpa` (optional) | A string of equalities/inequalities to filter by computed GPA. | `gpa=gte.3.5` |
| `graded` (optional) | A string of equalities/inequalities to filter by how many students received a letter grade. Useful for excluding sections too small to draw conclusions from. | `graded=gte.30` |
| `limit` (optional) | Maximum number of records to return; defaults to 100, maximum of 500. | `limit=10` |
| `offset` (optional) | How many records to skip when returning results; defaults to 0. | `offset=10` |
| `sortBy` (optional) | A comma-separated list of which columns to sort by, ascending (`.asc`) or descending (`.desc`). | `sortBy=term.desc,sec_code.asc` |

#### Output

| field | type | description |
| :-- | :--: | :-- |
| `term` | int | Six-digit term code: the four-digit year followed by the month the term begins (`01` spring, `08` fall). Fall 2024 is `202408`. |
| `course_code` | string | The course code, matching `course_code` elsewhere in this API. A course that has since been retired will have grade records but no entry in `/v1/courses`. |
| `sec_code` | string | The section code, matching `sec_code` on `/v1/sections`. |
| `instructor` | string or null | The instructor exactly as the Registrar printed them, in "Last, First Middle" order. Null where the release left the field blank. |
| `instructor_name` | string or null | The effective instructor in "First Last" order, suitable for matching against `/v1/instructors`. May be populated where `instructor` is null; see `instructor_source`. |
| `instructor_source` | string or null | How `instructor_name` was determined. `reported` means the Registrar named them on this row. `lead` means the name was carried from the lead section of the same lecture, which is how the release records discussion and lab sections. `course` means it was carried from a different lecture group or a differently-coded offering, and is materially less reliable. Null where no section of the course was named. |
| `total` | int | Students enrolled, as reported. From Fall 2017 this equals the sum of the fifteen grade buckets; in earlier terms it can exceed that sum by a few students whose outcome the older report did not categorize. Prefer `graded` as a denominator when comparing across that boundary. |
| `a_plus`, `a`, `a_minus` … `d_minus`, `f` | int | Students receiving each letter grade. |
| `w` | int | Students who withdrew. |
| `other` | int | Students receiving a non-letter outcome (pass/fail, incomplete, audit, and similar). |
| `graded` | int | Students who received a letter grade; the denominator used for `gpa`. |
| `gpa` | number or null | Mean GPA on the UMD 4.0 scale over `graded` students. Withdrawals and non-letter outcomes are excluded from both the numerator and the denominator. Null where nobody received a letter grade. |

#### Examples

##### Getting every section of a course in one term

Request: `GET http://api.jupiterp.com/v1/grades?courseCodes=CMSC132&term=eq.202408&limit=2`

Response:
```
[
  {
    "term": 202408,
    "course_code": "CMSC132",
    "sec_code": "0101",
    "instructor": "Herman, Larry",
    "instructor_name": "Larry Herman",
    "instructor_source": "reported",
    "total": 32,
    "a_plus": 0,
    "a": 1,
    "a_minus": 4,
    "graded": 30,
    "gpa": 2.583
  },
  {
    "term": 202408,
    "course_code": "CMSC132",
    "sec_code": "0102",
    "instructor": null,
    "instructor_name": "Larry Herman",
    "instructor_source": "lead",
    "total": 34,
    "a_plus": 2,
    "a": 7,
    "a_minus": 5,
    "graded": 34,
    "gpa": 3.118
  }
]
```

Note the second record: the release lists the instructor once against the lecture and leaves the discussion sections blank, so `instructor` is null while `instructor_name` carries the lecturer's name and `instructor_source` records that it was inferred.

### `/v1/grades/summary` 

[(back to endpoints)](#endpoints)

Gets grade distributions with the individual sections summed together. This is usually the endpoint you want: `groupBy=course` answers "how hard is this course", `groupBy=term` answers "has it changed", `groupBy=instructor` answers "who should I take it with", and `groupBy=instructorOverall` answers "how does this professor grade in general".

Note that `instructorOverall` and `instructorTerm` aggregate across every course, so they take no course filter; passing `courseCodes`, `prefix`, or `number` with them returns 400 rather than silently ignoring the filter.

#### Query parameters

| param | description | example |
|:--|:--|:--|
| `groupBy` (optional) | One of `course` (default), `term`, `instructor`, `instructorOverall`, or `instructorTerm`. `course` returns one record per course across every term on file; `term` one record per course per term; `instructor` one record per course per instructor; `instructorOverall` one record per instructor across every course they have taught; `instructorTerm` one record per instructor per term. | `groupBy=instructorOverall` |
| `includeCarried` (optional) | Only meaningful with `groupBy=instructor`. When true, also counts sections whose instructor was carried across lecture groups (`instructor_source` of `course`). Wider coverage, lower confidence. Defaults to false. | `includeCarried=true` |
| `courseCodes` (optional) | A string of one or multiple comma-separated course codes; cannot be combined with `prefix` or `number`. | `courseCodes=CMSC132` |
| `prefix` (optional) | The course prefix to match records to. | `prefix=CMSC3` |
| `number` (optional) | The course number to search for across multiple departments. | `number=433` |
| `term` (optional) | Equalities/inequalities to filter by term code. Only valid with `groupBy=term` or `groupBy=instructorTerm`; the other groupings aggregate across every term on file and will reject this parameter rather than ignore it. | `term=gte.202008` |
| `instructor` (optional) | Return only the given instructor, in "First Last" order. Case-sensitive, exact. **Prefer `instructorSlug`:** the same professor is spelled several different ways across the registrar's grade files, Testudo, and PlanetTerp, so an exact name match silently returns nothing for a large share of instructors. Requires an instructor grouping. | `instructor=Anwar%20Mamat` |
| `instructorSlug` (optional) | Return only the given instructor, by Jupiterp slug. This resolves through instructor identity rather than string equality, so it cannot miss because of a middle name or an accent. Requires an instructor grouping. | `instructorSlug=shane-walsh` |
| `instructorId` (optional) | Return only the given instructor, by numeric id. Requires an instructor grouping. | `instructorId=4711` |
| `gpa` (optional) | Equalities/inequalities to filter by the aggregated GPA. | `gpa=gte.3.0` |
| `minStudents` (optional) | Exclude groups with fewer than this many students who received a letter grade. Applied to `graded`, not `total`: before Fall 2017 the registrar's total includes students whose outcome was never categorized, so it is not comparable across eras, while `graded` is also the GPA denominator. | `minStudents=100` |
| `count` (optional) | If true, the total number of matching records is returned in the `Content-Range` response header. | `count=true` |
| `limit` (optional) | Maximum number of records to return; defaults to 100, maximum of 500. | `limit=10` |
| `offset` (optional) | How many records to skip; defaults to 0. | `offset=10` |
| `sortBy` (optional) | A comma-separated list of which columns to sort by. | `sortBy=gpa.desc` |

#### Output

All groupings return the summed grade buckets (`a_plus` through `other`), `total`, `graded`, and `gpa`, defined exactly as on `/v1/grades`. In addition:

| field | type | description |
| :-- | :--: | :-- |
| `course_code` | string | The course these counts are for. |
| `term` | int | Only present when `groupBy=term`. |
| `instructor` | string | Present on the instructor groupings; the instructor's canonical display name. |
| `instructor_id` | int | Present on the instructor groupings; the Jupiterp instructor id. |
| `instructor_slug` | string | Present on the instructor groupings; the Jupiterp slug, which is the professor page URL segment. |
| `course_count` | int | Only present when `groupBy=instructorOverall` or `instructorTerm`; how many distinct courses are represented. |
| `section_count` | int | How many individual sections were summed. |
| `term_count` | int | How many distinct terms are represented. Not present when `groupBy=term`. |
| `first_term`, `last_term` | int | The earliest and latest term represented. Not present when `groupBy=term`. |

#### Examples

##### How hard is a course, over its whole history

Request: `GET http://api.jupiterp.com/v1/grades/summary?courseCodes=CMSC351`

Response:
```
[
  {
    "course_code": "CMSC351",
    "section_count": 97,
    "term_count": 32,
    "first_term": 201008,
    "last_term": 202601,
    "total": 14969,
    "graded": 13346,
    "a_plus": 450,
    "a": 1578,
    "a_minus": 1153,
    "b_plus": 1313,
    "b": 2169,
    "b_minus": 1441,
    "c_plus": 1263,
    "c": 1583,
    "c_minus": 1002,
    "d_plus": 185,
    "d": 846,
    "d_minus": 70,
    "f": 293,
    "w": 738,
    "other": 791,
    "gpa": 2.699
  }
]
```

##### Comparing instructors for a course

Request: `GET http://api.jupiterp.com/v1/grades/summary?groupBy=instructor&courseCodes=CMSC330&minStudents=1000&sortBy=gpa.desc`

Response:
```
[
  {
    "course_code": "CMSC330",
    "instructor": "Michael W. Hicks",
    "section_count": 37,
    "term_count": 7,
    "first_term": 201301,
    "last_term": 202101,
    "total": 1205,
    "graded": 1076,
    "gpa": 3.123
  },
  {
    "course_code": "CMSC330",
    "instructor": "Roger D. Eastman",
    "section_count": 39,
    "term_count": 5,
    "first_term": 201808,
    "last_term": 202108,
    "total": 1258,
    "graded": 1045,
    "gpa": 3.047
  }
]
```

### `/v1/grades/terms` 

[(back to endpoints)](#endpoints)

Gets every term for which grade data has been loaded, newest first. Takes no parameters. Useful for discovering coverage before querying, since the released data covers fall and spring only.

#### Output

| field | type | description |
| :-- | :--: | :-- |
| `term` | int | Six-digit term code. |
| `section_count` | int | Sections with grade data in this term. |
| `course_count` | int | Distinct courses with grade data in this term. |
| `total` | int | Students enrolled across every section. |
| `graded` | int | Students who received a letter grade. |
| `gpa` | number | Mean GPA across the whole university for the term. |

#### Example

Request: `GET http://api.jupiterp.com/v1/grades/terms`

Response:
```
[
  {
    "term": 202601,
    "section_count": 6543,
    "course_count": 3213,
    "total": 168366,
    "graded": 161397,
    "gpa": 3.488
  },
  {
    "term": 202508,
    "section_count": 7056,
    "course_count": 3263,
    "total": 186608,
    "graded": 176931,
    "gpa": 3.503
  }
]
```
---

# Jupiterp API v1 (reviews)

The endpoints below write, and they are governed differently from the read
endpoints above even though both are served under `/v1`. Writes need things
reads do not: an origin allowlist, authentication, rate limiting, and a captcha.
The read endpoints stay permissive, unauthenticated, and cacheable.

That difference is worth stating plainly, because it is the one thing the shared
prefix hides. **Reads accept requests from any origin. Writes accept them only
from an allowlist** (`V1_ALLOWED_ORIGINS`). A browser on an unrelated domain can
call `GET /v1/courses` and will be refused by `POST /v1/reviews`.

Reviews are **pre-moderated**. Nothing submitted here is publicly visible until
a moderator approves it, and that is true whether the decision is made by a
person or by the automated triage.

| path | method | description |
| :-- | :-- | :-- |
| `/v1/reviews` | GET | Approved reviews for a professor |
| `/v1/reviews` | POST | Submit a review |
| `/v1/reviews/verify/:token` | GET | Confirm an emailed link |
| `/v1/reviews/:id` | DELETE | Withdraw (manage key) |
| `/v1/reviews/:id/report` | POST | Report a published review |
| `/v1/admin/reviews` | GET | Moderation queue (admin key) |
| `/v1/admin/reviews/:id` | PUT | Approve, reject, or escalate |
| `/v1/admin/reports` | GET | Open reports (admin key) |
| `/v1/admin/sweep` | POST | Scheduled maintenance (admin key) |

## `GET /v1/reviews`

Approved reviews only, newest first. Served from a database view that cannot
express an unapproved row and does not contain the submitter's identity
columns at all.

| parameter | description | example |
| :-- | :-- | :-- |
| `instructorSlug` (required) | Whose reviews to return. | `instructorSlug=shane-walsh` |
| `courseCode` (optional) | Restrict to one course. | `courseCode=CMSC132` |
| `limit`, `offset` (optional) | Paging; defaults 25 and 0. | `limit=50` |

The total is returned in the `Content-Range` header.

## `POST /v1/reviews`

```json
{
  "instructor_slug": "shane-walsh",
  "course_code": "CMSC132",
  "term": 202508,
  "rating": 4.5,
  "expected_grade": "A-",
  "title": "Genuinely excellent lecturer",
  "body": "…",
  "email": "student@terpmail.umd.edu",
  "captcha_token": "0.abc…"
}
```

`rating` is a decimal between 1 and 5 **on a half step** — `4.5` is valid,
`4.3` is not. `email` must be a `terpmail.umd.edu` or `umd.edu` address; it is
stored only as a peppered hash, is never displayed, and is never shown to the
professor. `course_code` and `term` are optional, and `term` must be a Fall or
Spring term, because the grade dataset covers only those.

Responds `202 Accepted` with `{"status":"verification_sent"}`.

**The response is identical whether or not that address has already reviewed
this professor.** A distinguishable "you have already reviewed this" would turn
the endpoint into an oracle for "did person X review professor Y", which is the
privacy property the hashing exists to provide.

Rate limited to 5 per hour per IP, 3 per day per address, and 20 per hour per
professor across all submitters. The last one is what catches a coordinated
run on a single professor, which the per-person limits do nothing about.

## `GET /v1/reviews/verify/:token`

Confirms the emailed link, moves the review to `pending`, and returns the
manage key once:

```json
{ "status": "verified", "manage_key": "…", "message": "…" }
```

Idempotent: a second visit returns `already_verified` rather than an error,
because mail clients prefetch links and people double-click.

The manage key is also emailed. It cannot be recovered — there is deliberately
no way to link it back to a person.

## `DELETE /v1/reviews/:id`

`Authorization: Bearer <manage key>`.

A withdrawal is a soft delete — the row remains so the one-review-per-person
rule still holds, but the content is actually nulled.

There is no edit endpoint. A published review is final text: the only way to
change what a review says is to withdraw it and write another. Editing after
approval is a way to get innocuous text past a moderator and then replace it,
and re-queueing every edit for moderation solves that at the cost of a flow
where a reviewer can silently republish. Withdrawal carries no such hole, so it
is the one the reviewer keeps.

## `PUT /v1/admin/reviews/:id`

```json
{
  "action": "approve",
  "reason": "…",
  "confidence": 0.93,
  "categories": [],
  "policy_version": "2026-08-14",
  "model": "gemini-2.0-flash-001"
}
```

Two callers with different keys: a human moderator with the admin key, and the
automated triage with a narrowly scoped callback key that authorises this one
route. Which one acted is recorded on every decision.

Idempotent — asking for the state a review is already in is a success, not a
second audit entry. State-guarded — only `pending` and `escalated` reviews are
decidable, and a late retry against a review a human already actioned returns
`409` rather than overturning it.

Every call writes an audit row. While shadow mode is on, an automated decision
is recorded with `applied: false` and the review is escalated to a human
instead.

## Errors

| status | meaning |
| :-- | :-- |
| `400` | Validation failed; the message names the field |
| `401` | Missing or wrong key |
| `404` | No such professor or review |
| `409` | Already decided by someone else |
| `410` | Verification link expired |
| `429` | Rate limited |
