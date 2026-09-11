from __future__ import annotations

import pathlib
import tempfile
import unittest

from check_built_site import local_target


class LocalTargetTest(unittest.TestCase):
    def test_strips_the_published_subpath_from_root_relative_urls(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            site = pathlib.Path(temporary) / "docs"
            page = site / "usage" / "example" / "index.html"

            target, fragment = local_target(
                site,
                page,
                "/docs/assets/stylesheets/main.css#theme",
                "/docs",
            )

            self.assertEqual(target, (site / "assets/stylesheets/main.css").resolve())
            self.assertEqual(fragment, "theme")

    def test_ignores_root_routes_owned_by_another_site_tier(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            site = pathlib.Path(temporary) / "docs"
            page = site / "index.html"

            self.assertIsNone(local_target(site, page, "/guide/", "/docs"))


if __name__ == "__main__":
    unittest.main()
