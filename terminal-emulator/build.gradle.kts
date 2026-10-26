plugins {
    alias(libs.plugins.android.library)
}

android {
    namespace = "com.termux.emulator"
    compileSdk = 36

    defaultConfig {
        minSdk = 26
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    testOptions {
        // The emulator tests touch android.util.Log through Logger.
        unitTests.isReturnDefaultValues = true
    }
}

dependencies {
    implementation("androidx.annotation:annotation:1.9.0")

    testImplementation(libs.junit)
}
