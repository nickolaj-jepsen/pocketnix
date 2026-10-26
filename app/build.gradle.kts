plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
}

android {
    namespace = "dev.nickolaj.pocketnix"
    compileSdk {
        version = release(36) {
            minorApiLevel = 1
        }
    }

    defaultConfig {
        applicationId = "dev.nickolaj.pocketnix"
        // android.system.virtualmachine arrived in API 34.
        minSdk = 34
        targetSdk = 36
        versionCode = 1
        versionName = "0.0.0"

        ndk {
            abiFilters += "arm64-v8a"
        }
    }

    // CI passes the release key as Gradle properties; without them release builds stay unsigned.
    signingConfigs {
        create("release") {
            val storePath = project.findProperty("POCKETNIX_RELEASE_STORE_FILE") as? String
            if (storePath != null && file(storePath).exists()) {
                storeFile = file(storePath)
                storePassword = project.findProperty("POCKETNIX_RELEASE_STORE_PASSWORD") as? String
                keyAlias = project.findProperty("POCKETNIX_RELEASE_KEY_ALIAS") as? String
                keyPassword = project.findProperty("POCKETNIX_RELEASE_KEY_PASSWORD") as? String
            }
        }
    }

    buildTypes {
        debug {
            applicationIdSuffix = ".debug"
            versionNameSuffix = "-debug"
        }
        release {
            signingConfig = signingConfigs.getByName("release")
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        compose = true
    }
}

dependencies {
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.compose.ui)
    implementation(libs.androidx.compose.material3)
    implementation(libs.kotlinx.coroutines.android)
    // The AVF API is @SystemApi and VirtualMachineCustomImageConfig is @hide; this lifts the
    // hidden-API check for this process only (a global adb exemption would cover every app).
    implementation(libs.hiddenapibypass)
    implementation(project(":terminal-view"))

    testImplementation(libs.junit)
}
