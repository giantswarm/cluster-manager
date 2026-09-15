"""ATS smoke for the cluster-manager chart.

app-test-suite (>= 1.0) installs the packaged chart on the job's kind cluster
with `helm upgrade --install --wait` (tests/test-values.yaml, namespace from
.ats/main.yaml) and then runs this file with `pytest -m smoke`. This skeleton
chart has no templates yet; the smoke checks the install path end to end (the
cluster is reachable, the empty release installs). The Deployment readiness
check arrives with the server and its templates.
"""

import logging
import os

import pykube
import pytest
from pytest_helm_charts.clusters import Cluster

logger = logging.getLogger(__name__)

NAMESPACE = os.environ.get("ATS_RELEASE_NAMESPACE", "agent-platform")


@pytest.mark.smoke
def test_api_working(kube_cluster: Cluster) -> None:
    """The kind cluster ATS runs against is reachable."""
    assert kube_cluster.kube_client is not None
    assert len(pykube.Node.objects(kube_cluster.kube_client)) >= 1
