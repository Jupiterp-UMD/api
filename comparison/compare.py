import requests
from bs4 import BeautifulSoup

# Compare course catalog
jupiterp_courses = []
offset = 0

while True:
    print(f"sending request to Jupiterp API; offset={offset}")
    url = f"http://api.jupiterp.com/v0/courses/minified?limit=500&offset={offset}"
    batch = requests.get(url).json()
    if not batch:
        break
    jupiterp_courses.extend(batch)
    offset += 500

jupiterp_codes = [course['course_code'] for course in jupiterp_courses]

print(f"Found {len(jupiterp_codes)} Jupiterp course codes.")

umdio_courses = []

url = f"http://api.umd.io/v1/courses/list?semester=202508"
batch = requests.get(url).json()
umdio_courses.extend(batch)

umdio_codes = [course['course_id'] for course in umdio_courses]

print(f"Found {len(umdio_codes)} UMD.io course codes.")

only_in_jupiterp = set(jupiterp_codes) - set(umdio_codes)
only_in_umdio = set(umdio_codes) - set(jupiterp_codes)

print(f"Courses only in Jupiterp ({len(only_in_jupiterp)}): {sorted(only_in_jupiterp)}")
print(f"Courses only in UMD.io ({len(only_in_umdio)}): {sorted(only_in_umdio)}")
