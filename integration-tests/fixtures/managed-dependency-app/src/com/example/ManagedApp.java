// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package com.example;

import com.example.approved.Greeting;

public final class ManagedApp {
    private ManagedApp() {
    }

    public static void main(String[] args) {
        System.out.println(Greeting.message());
    }
}
