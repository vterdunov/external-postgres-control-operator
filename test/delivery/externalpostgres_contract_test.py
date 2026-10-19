"""Public descriptor API in the artifacts administrators actually install."""

import re

from support import CHART, DeliveryCase, documents, kind


class ExternalPostgresContractTests(DeliveryCase):
    def assert_descriptor_api(self, rendered):
        crds = [doc for doc in documents(rendered) if kind(doc) == "CustomResourceDefinition"]
        names = [re.search(r"(?m)^  name:\s*(\S+)", doc).group(1) for doc in crds]
        self.assertEqual(set(names), {
            "externalpostgres.postgresql.epco.io",
            "databaseroles.postgresql.epco.io",
            "databases.postgresql.epco.io",
        })
        descriptor = next(doc for doc in crds if re.search(
            r"(?m)^  name: externalpostgres\.postgresql\.epco\.io$", doc))
        for field, value in (("kind", "ExternalPostgres"), ("listKind", "ExternalPostgresList"),
                             ("plural", "externalpostgres"), ("singular", "externalpostgres")):
            self.assertRegex(descriptor, rf"(?m)^    {field}: {value}$")
        self.assertRegex(descriptor, r"(?m)^    shortNames:\n    - epg$")
        roles = [doc for doc in documents(rendered) if kind(doc) == "ClusterRole"]
        manager = next(doc for doc in roles if re.search(r"(?m)^  name: epco-manager-role$", doc))
        for resource in ("externalpostgres", "externalpostgres/status"):
            self.assertRegex(manager, r"(?m)^\s+- " + re.escape(resource) + r"$")
        self.assertNotRegex(manager, r"(?m)^\s+- postgresclusters(?:/[^\s]+)?$")

    def test_helm_installs_only_the_renamed_descriptor_api(self):
        result = self.helm("template", "epco", str(CHART), "--namespace", "epco-system", "--include-crds")
        self.assert_success(result)
        self.assert_descriptor_api(result.stdout)

    def test_installer_installs_only_the_renamed_descriptor_api(self):
        result = self.run_cli("bash", "tools/deploy.sh", "build-installer", environment={"KUBE_CONTEXT": ""})
        self.assert_success(result)
        self.assert_descriptor_api((self.repo / "dist/install.yaml").read_text())
