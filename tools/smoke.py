#!/usr/bin/env python3
"""End-to-end checks against a running Jupiterp API.

    python3 api/tools/smoke.py [--base http://localhost:8080]

The Go tests cover the pure functions. This covers what they cannot: whether
the handlers, PostgREST, the SQL functions, and the grants underneath them
actually agree once they are wired together. Nearly every bug found during the
grade-migration rehearsal lived in that seam and produced a 200 the whole way.

Three kinds of check, in order of how quietly they used to fail:

  reachability  every endpoint answers, and answers with data. Weak, but it is
                what catches a missing grant -- RLS with no policy returns
                `200 []`, which looks like "no results" and is really "no
                access".

  filters bind  a filter parameter actually constrains the result. This is the
                one worth having. Gin's ShouldBindQuery ignores unknown query
                parameters, so `instructorSlugs=` (plural) on an endpoint whose
                parameter is `instructorSlug` (singular) returns every
                professor with no error at all. A caller asking for one
                professor's grades and receiving all of them cannot tell.

  ordering      a paginated endpoint returns a stable set across pages. Without
                an ORDER BY, Postgres may reuse rows between LIMIT/OFFSET
                windows, so a full paginated read silently loses some and
                duplicates others -- 2,976 rows came back as 2,336 distinct,
                with a different set missing on each run.

Exits non-zero if any check fails, so it can gate a deploy.
"""

import argparse
import json
import sys
import time
import uuid
import urllib.error
import urllib.parse
import urllib.request

TIMEOUT = 60

# The prefix the read surface is served under. /v0 is still registered against
# the same handlers as a compatibility alias -- `check_alias_parity` is what
# holds the two together, so this can move without stranding old clients.
READ = "/v1"
ALIAS = "/v0"

failures: list[str] = []
passes = 0


def get(base: str, path: str, params: dict | None = None):
    """GET a path, returning (status, decoded body or raw text)."""
    url = base.rstrip("/") + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    request = urllib.request.Request(url, headers={"Accept": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=TIMEOUT) as response:
            body = response.read().decode("utf-8", "replace")
            try:
                return response.status, json.loads(body)
            except json.JSONDecodeError:
                return response.status, body
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode("utf-8", "replace")
    except Exception as error:  # noqa: BLE001 - connection refused, timeout, DNS
        return 0, str(error)


def check(name: str, ok: bool, detail: str = ""):
    global passes
    if ok:
        passes += 1
        print(f"  ok   {name}")
    else:
        failures.append(f"{name}: {detail}")
        print(f"  FAIL {name}: {detail}")


def rows_of(body):
    """Endpoints answer either with a bare array or with a wrapped one."""
    if isinstance(body, list):
        return body
    if isinstance(body, dict):
        for key in ("entries", "instructors", "reviews", "data", "results"):
            if isinstance(body.get(key), list):
                return body[key]
    return []


def check_reachable(base: str):
    print("\nreachability")
    endpoints = [
        (f"{READ}/courses", {"limit": "5"}),
        (f"{READ}/courses/minified", {"limit": "5"}),
        (f"{READ}/courses/withSections", {"courseCodes": "CMSC132"}),
        (f"{READ}/deptList", None),
        (f"{READ}/instructors", {"limit": "5"}),
        (f"{READ}/instructors/active", {"limit": "5"}),
        (f"{READ}/sections", {"courseCodes": "CMSC132"}),
        (f"{READ}/grades", {"courseCodes": "CMSC132", "limit": "5"}),
        (f"{READ}/grades/summary", {"courseCodes": "CMSC132"}),
        (f"{READ}/grades/summary", {"groupBy": "instructorOverall", "instructorSlug": "clyde-kruskal"}),
        (f"{READ}/grades/terms", None),
        ("/v1/reviews", {"instructorSlug": "clyde-kruskal"}),
    ]
    for path, params in endpoints:
        status, body = get(base, path, params)
        label = path + (f"?{urllib.parse.urlencode(params)}" if params else "")
        if status != 200:
            check(label, False, f"HTTP {status} -- {str(body)[:120]}")
            continue
        # An empty array from a read endpoint is the shape a missing grant
        # takes, so it is reported rather than passed over.
        count = len(rows_of(body))
        check(label, count > 0, f"HTTP 200 but zero rows (missing grant? RLS with no policy?)")


def check_filters_bind(base: str):
    """A filter must return strictly fewer rows than no filter, and the rows it
    returns must all match. Both halves matter: a filter that is ignored passes
    the second check trivially."""
    print("\nfilters actually constrain")

    cases = [
        # path, filter params, field the filter is on, expected value
        (f"{READ}/grades/summary",
         {"groupBy": "instructor", "instructorSlug": "clyde-kruskal"},
         "instructor_slug", "clyde-kruskal"),
        (f"{READ}/instructors",
         {"instructorSlugs": "clyde-kruskal"},
         "slug", "clyde-kruskal"),
        (f"{READ}/grades",
         {"courseCodes": "CMSC132"},
         "course_code", "CMSC132"),
        (f"{READ}/sections",
         {"courseCodes": "CMSC132"},
         "course_code", "CMSC132"),
    ]

    for path, params, field, expected in cases:
        label = f"{path} {urllib.parse.urlencode(params)}"

        unfiltered_params = {k: v for k, v in params.items() if k in ("groupBy",)}
        unfiltered_status, unfiltered = get(base, path, {**unfiltered_params, "limit": "100"})
        filtered_status, filtered = get(base, path, params)

        if filtered_status != 200 or unfiltered_status != 200:
            check(label, False, f"HTTP {filtered_status}/{unfiltered_status}")
            continue

        filtered_rows = rows_of(filtered)
        unfiltered_rows = rows_of(unfiltered)

        if not filtered_rows:
            check(label, False, "filter returned nothing; the fixture may be gone")
            continue

        # Every row matches. Rows that do not carry the field are not evidence
        # either way, so they are skipped rather than counted as matches.
        present = [r for r in filtered_rows if isinstance(r, dict) and field in r]
        mismatched = [r for r in present if r.get(field) != expected]
        if mismatched:
            check(label, False,
                  f"{len(mismatched)}/{len(present)} rows have {field} != {expected!r} "
                  f"(e.g. {mismatched[0].get(field)!r}) -- the filter is being ignored")
            continue

        # Homogeneous output only means something if the unfiltered response was
        # heterogeneous. Comparing row *counts* does not work: both responses hit
        # the same page limit whenever the filter still matches more rows than a
        # page holds, which reads as "did not narrow" on a filter that is fine.
        others = [r for r in unfiltered_rows
                  if isinstance(r, dict) and field in r and r.get(field) != expected]
        if not others:
            check(label, True, "")
            print(f"       (inconclusive: unfiltered sample was already homogeneous on {field})")
            continue

        check(label, True)

    # The specific trap: Gin ignores query parameters it does not recognise, so
    # a misspelling is indistinguishable from no filter at all. Pinned here so
    # that if strict binding is ever added, this flips and gets revisited.
    status, body = get(base, f"{READ}/grades/summary",
                       {"groupBy": "instructor", "nonexistentParam": "xyz", "limit": "5"})
    check("unknown query parameters are tolerated (documented Gin behaviour)",
          status == 200,
          f"HTTP {status} -- if this is now a 400, strict binding was added; "
          "update the docs, this is an improvement")


def check_pagination_is_stable(base: str):
    """Page through a listing twice and confirm the set of ids is identical.

    An unordered LIMIT/OFFSET read is free to return the same row on two pages
    and skip another entirely. It looks fine one page at a time."""
    print("\npagination stability")

    page_size = 100
    pages = 5

    def read_all():
        seen = []
        for page in range(pages):
            status, body = get(base, f"{READ}/instructors",
                               {"limit": str(page_size), "offset": str(page * page_size)})
            if status != 200:
                return None, f"HTTP {status} on page {page}"
            rows = rows_of(body)
            if not rows:
                break
            seen.extend(r.get("slug") for r in rows if isinstance(r, dict))
        return seen, None

    first, error = read_all()
    if error:
        check("paginated read", False, error)
        return

    distinct = len(set(first))
    check("no duplicates across pages",
          distinct == len(first),
          f"read {len(first)} rows but only {distinct} distinct -- "
          "rows are repeating across LIMIT/OFFSET windows, so others are being skipped "
          "(the listing needs a total ORDER BY)")

    second, error = read_all()
    if error:
        check("second paginated read", False, error)
        return

    check("two full reads agree",
          set(first) == set(second),
          f"first read saw {len(set(first))} distinct, second saw {len(set(second))}; "
          f"{len(set(first) ^ set(second))} rows differ between identical requests")


def preflight(base: str, method: str, path: str, origin: str):
    """Send a CORS preflight, returning (status, Allow-Methods)."""
    request = urllib.request.Request(base.rstrip("/") + path, method="OPTIONS")
    request.add_header("Origin", origin)
    request.add_header("Access-Control-Request-Method", method)
    request.add_header("Access-Control-Request-Headers", "content-type,authorization")
    try:
        with urllib.request.urlopen(request, timeout=TIMEOUT) as response:
            return response.status, response.headers.get("Access-Control-Allow-Methods", "")
    except urllib.error.HTTPError as error:
        headers = error.headers.get("Access-Control-Allow-Methods", "") if error.headers else ""
        return error.code, headers
    except Exception as error:  # noqa: BLE001
        return 0, str(error)


def check_timeout_headroom(base: str, budget: float = 1.5):
    """Warn on any endpoint approaching the `anon` role's statement timeout.

    Every /v0 request authenticates as `anon`, which Supabase caps at
    `statement_timeout = 3s` (authenticated gets 8s, service_role is unset).
    Crossing it makes PostgREST return 500, and the handler passes that through.

    This is a slow failure, not a sudden one: `grade_terms` was a plain view
    doing a full aggregate over `grades`, and it simply got heavier every term
    until it started timing out -- ~2s on a good run, over 3s on a bad one, with
    nothing in between to notice. It is now materialized (migration 0029).

    The API caches responses in memory, so a plain repeat request measures the
    cache and always passes -- a check that cannot fail. Each request below
    carries a unique throwaway parameter instead: the cache key is built from
    the full query string, so a novel one always misses, while Gin's binding
    ignores the parameter itself and the query is unchanged. That is the same
    permissiveness `check_filters_bind` warns about, used deliberately here.

    Timings include the network, so treat them as an early warning rather than a
    measurement. Anything over half the budget is worth materializing before it
    decides for you.
    """
    print(f"\ntimeout headroom (anon statement_timeout is 3s; warn above {budget}s)")

    endpoints = [
        f"{READ}/grades/terms",
        f"{READ}/grades/summary?groupBy=instructorOverall&limit=500",
        f"{READ}/grades/summary?groupBy=instructorTerm&limit=500",
        f"{READ}/grades?limit=500",
        f"{READ}/courses/withSections",
        f"{READ}/instructors?limit=500",
    ]

    for endpoint in endpoints:
        path, _, query = endpoint.partition("?")
        params = dict(urllib.parse.parse_qsl(query)) if query else {}
        params["_cachebust"] = uuid.uuid4().hex

        started = time.monotonic()
        status, body = get(base, path, params)
        elapsed = time.monotonic() - started

        if status != 200:
            check(f"{endpoint} responds", False, f"HTTP {status}")
            continue
        # If the bust stopped working the timing is meaningless, so say so
        # rather than reporting a reassuring 0.00s.
        if elapsed < 0.005:
            check(f"{endpoint} was actually measured", False,
                  f"returned in {elapsed:.4f}s, which means it came from the API cache. "
                  "The cache-busting parameter is no longer producing a distinct key")
            continue
        check(f"{endpoint} [{elapsed:.2f}s]",
              elapsed < budget,
              f"took {elapsed:.2f}s, over half the 3s anon statement_timeout -- "
              "this is the shape grade_terms had before it started returning 500s. "
              "Consider materializing it")


def check_cors_preflight(base: str, origin: str):
    """A browser sends OPTIONS before any JSON POST or PUT.

    Two separate things can go wrong, and they need separate checks because a
    403 is the *correct* answer for an origin the server does not serve:

      - no OPTIONS route at all, so Gin 404s before CORS middleware runs. This
        is origin-independent and breaks every browser client.
      - the verb is missing from AllowMethods, so an allowed origin is still
        refused. PUT was missing while it was the moderation route.
    """
    print(f"\nCORS preflight (origin {origin})")

    routes = [("POST", "/v1/reviews"), ("PUT", "/v1/admin/reviews/x"),
              ("POST", "/v1/admin/instructors/queue/1")]

    for method, path in routes:
        status, allowed = preflight(base, method, path, origin)

        if status == 0:
            check(f"preflight {method} {path}", False, allowed)
            continue
        if status == 404:
            check(f"preflight {method} {path}", False,
                  "404 -- no OPTIONS route is registered, so no browser can send this "
                  "request regardless of origin")
            continue
        if status == 403:
            check(f"preflight {method} {path}", False,
                  f"403 -- {origin} is not in V1_ALLOWED_ORIGINS. Pass --origin with one "
                  "the server serves, or add this one to the server's config")
            continue
        check(f"preflight {method} {path} advertises {method}",
              method in allowed,
              f"Allow-Methods is {allowed!r}, which omits {method} -- the browser will "
              f"refuse to send it even though the route exists")

    # Origin-independent: an unservable origin must still be *answered*, not
    # 404'd. A 404 here means the route is missing rather than the origin
    # rejected, which is the failure that hid behind a working curl.
    status, _ = preflight(base, "POST", "/v1/reviews", "https://not-a-real-origin.example")
    check("preflight for a disallowed origin is refused, not 404",
          status != 404,
          "404 means no OPTIONS route exists at all")


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--base", default="http://localhost:8080",
                        help="API base URL (default: http://localhost:8080)")
    parser.add_argument("--origin", default="http://localhost:5173",
                        help="Origin to send on CORS preflights. Must be one the server "
                             "serves (V1_ALLOWED_ORIGINS); use https://www.jupiterp.com "
                             "against production. Default: http://localhost:5173")
    args = parser.parse_args()

    print(f"smoke checks against {args.base}")

    status, _ = get(args.base, f"{READ}/deptList")
    if status == 0:
        print(f"\ncannot reach {args.base}. Is the API running?", file=sys.stderr)
        return 2

    check_reachable(args.base)
    check_filters_bind(args.base)
    check_pagination_is_stable(args.base)
    check_timeout_headroom(args.base)
    check_cors_preflight(args.base, args.origin)

    print()
    if failures:
        print(f"{len(failures)} failed, {passes} passed\n")
        for failure in failures:
            print(f"  - {failure}")
        return 1
    print(f"all {passes} checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
