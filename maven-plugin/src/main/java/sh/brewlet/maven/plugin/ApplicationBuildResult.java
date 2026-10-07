// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.project.MavenProject;
import sh.brewlet.maven.plugin.model.JvmConfig;

/** Per-project handoff between goals in one Maven invocation; never loaded from disk. */
record ApplicationBuildResult(String reference, String digest, String format, JvmConfig config) {
    private static final String KEY = ApplicationBuildResult.class.getName();

    static ApplicationBuildResult get(MavenProject project) {
        Object result = project.getContextValue(KEY);
        return result instanceof ApplicationBuildResult built ? built : null;
    }

    static void clear(MavenProject project) {
        project.setContextValue(KEY, null);
    }

    void save(MavenProject project) {
        project.setContextValue(KEY, this);
    }
}
