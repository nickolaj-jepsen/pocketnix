package dev.nickolaj.pocketnix.managed

import dev.nickolaj.pocketnix.Settings
import java.io.File
import java.util.zip.GZIPInputStream
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext

/** One nixpkgs package: its attribute (what `nixpkgs#<attr>` installs), version and description. */
data class Package(val attr: String, val version: String, val description: String)

/**
 * The release's list of the packages in its nixpkgs, which the app searches
 * offline. Downloaded once per release, then kept.
 */
class PackageIndex(private val dir: File, private val settings: Settings) {
    private val lock = Mutex()
    private var loaded: List<Package>? = null

    /** The packages, downloading the index first if this release's isn't here. */
    suspend fun packages(): List<Package> = lock.withLock {
        loaded ?: withContext(Dispatchers.IO) {
            val name = Release.packageIndex()
            val file = File(dir, name)
            if (!file.exists()) {
                dir.mkdirs()
                dir.listFiles()?.forEach { it.delete() }
                val part = File(dir, "$name.part")
                Release.download(Release.url(settings, name), part)
                part.renameTo(file)
            }
            parse(file)
        }.also { loaded = it }
    }

    companion object {
        /** Reads the index: gzipped lines of tab-separated attribute, version and description. */
        fun parse(file: File): List<Package> = GZIPInputStream(file.inputStream()).bufferedReader().useLines { lines ->
            lines.mapNotNull { line ->
                val f = line.split('\t', limit = 3)
                if (f.size < 2 || f[0].isEmpty()) null else Package(f[0], f[1], f.getOrElse(2) { "" })
            }.toList()
        }

        /**
         * The packages matching every word of [query] in their attribute or
         * description, attributes that match it best first.
         */
        fun search(packages: List<Package>, query: String, limit: Int = 50): List<Package> {
            val words = query.lowercase().split(' ').filter { it.isNotBlank() }
            if (words.isEmpty()) return emptyList()
            val q = query.trim().lowercase()
            return packages.asSequence()
                .filter { p -> words.all { w -> p.attr.lowercase().contains(w) || p.description.lowercase().contains(w) } }
                .sortedWith(
                    compareBy<Package>(
                        { p ->
                            val a = p.attr.lowercase()
                            val name = a.substringAfterLast('.')
                            when {
                                name == q || a == q -> 0
                                name.startsWith(q) -> 1
                                a.contains(q) -> 2
                                else -> 3
                            }
                        },
                        // Top-level attributes before ones in a package set.
                        { it.attr.count { c -> c == '.' } },
                        { it.attr.length },
                        { it.attr }
                    )
                )
                .take(limit)
                .toList()
        }
    }
}
