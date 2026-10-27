plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
}

// The CLI shares it (nix/go.nix): provision installs the release matching its own version.
val pocketnixVersion = rootProject.file("VERSION").readText().trim()

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
        // Android installs only a higher code over an existing app: major.minor.patch, two digits each.
        versionCode = pocketnixVersion.split(".").map { it.toInt() }.let { (major, minor, patch) ->
            require(minor < 100 && patch < 100) { "VERSION $pocketnixVersion doesn't fit versionCode" }
            major * 10000 + minor * 100 + patch
        }
        versionName = pocketnixVersion

        ndk {
            abiFilters += "arm64-v8a"
        }
    }

    // The GUI renderer's C half: zstd and AHardwareBuffer copies. The SDK's CMake, as AGP would download another.
    externalNativeBuild {
        cmake {
            path = file("src/main/cpp/CMakeLists.txt")
            version = "4.1.2"
        }
    }

    // CI passes the release key as Gradle properties; without them release builds stay unsigned.
    val releaseStore = project.findProperty("POCKETNIX_RELEASE_STORE_FILE") as? String
    signingConfigs {
        if (releaseStore != null) {
            // A wrong path would otherwise publish an unsigned release.
            require(file(releaseStore).isFile) { "POCKETNIX_RELEASE_STORE_FILE $releaseStore is not a file" }
            create("release") {
                storeFile = file(releaseStore)
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
            // The x86_64 emulator runs native code untranslated, so its timings and crashes are real.
            ndk {
                abiFilters += "x86_64"
            }
        }
        release {
            signingConfig = signingConfigs.findByName("release")
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
        buildConfig = true
    }

    testOptions {
        // Keys.kt reads android.view.KeyEvent constants, which compile inline; Log calls return defaults.
        unitTests.isReturnDefaultValues = true
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
    // The platform's org.json is a stub on the JVM; tests need a real one.
    testImplementation(libs.json)
    // A pure-Java zstd, to mirror a real compositor's commits on the JVM.
    testImplementation("io.airlift:aircompressor:0.27")
}
