// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package com.example.classpath;

/**
 * A deliberately non-modular helper that ships on the supplementary
 * class path in the mixed class-path + module-path demo (see
 * docs/layered-classpath-deployment.md §8.1). It lives in the unnamed module and
 * is unpacked to {@code /app/lib}, fed to {@code java -cp} alongside the module
 * path. Its mere presence, probed by {@code OrdersApp}, proves the mixed launch
 * assembled both {@code -cp} and {@code -p}.
 */
public final class ClasspathHelper {

    public static String banner() {
        return "non-modular helper on the class path";
    }

    private ClasspathHelper() {
    }
}
