// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.junit.jupiter.api.Test;

import javax.xml.parsers.DocumentBuilderFactory;
import java.util.HashSet;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.*;

class PluginSurfaceTest {
    @Test
    void descriptorExposesOfflineManifestButNoDeploymentGoal() throws Exception {
        try (var input = getClass().getResourceAsStream("/META-INF/maven/plugin.xml")) {
            assertNotNull(input);
            var factory = DocumentBuilderFactory.newInstance();
            factory.setFeature("http://apache.org/xml/features/disallow-doctype-decl", true);
            var document = factory.newDocumentBuilder().parse(input);
            Set<String> goals = new HashSet<>();
            var nodes = document.getElementsByTagName("mojo");
            for (int i = 0; i < nodes.getLength(); i++) {
                var mojo = (org.w3c.dom.Element) nodes.item(i);
                goals.add(mojo.getElementsByTagName("goal").item(0).getTextContent());
            }
            assertEquals(Set.of("config", "build", "push", "appcds", "dependency-bundle", "inspect", "help", "manifest"), goals);

            Set<String> removed = Set.of("kubeconfig", "kubeContext", "kubectl", "waitForReady", "waitTimeout");
            Set<String> manifestOnly = Set.of("namespace", "appName", "replicas", "ports", "probes", "readinessPath", "livenessPath",
                    "cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit", "jvmArgs", "jdkDistribution", "launcher");
            for (int i = 0; i < nodes.getLength(); i++) {
                var mojo = (org.w3c.dom.Element) nodes.item(i);
                String goal = mojo.getElementsByTagName("goal").item(0).getTextContent();
                var parameters = mojo.getElementsByTagName("parameter");
                for (int j = 0; j < parameters.getLength(); j++) {
                    var parameter = (org.w3c.dom.Element) parameters.item(j);
                    String name = parameter.getElementsByTagName("name").item(0).getTextContent();
                    assertFalse(removed.contains(name), "Cluster parameter still exposed: " + name);
                    if ("manifest".equals(goal)) {
                        assertFalse(Set.of("image", "registry", "jarFile", "format", "dependencyBundle")
                                .contains(name), "Manifest must use the current build, not an input: " + name);
                    } else {
                        assertFalse(manifestOnly.contains(name), goal + " exposes manifest-only parameter: " + name);
                    }
                }
            }
        }
    }
}
