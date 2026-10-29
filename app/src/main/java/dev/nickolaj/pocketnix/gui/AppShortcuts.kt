package dev.nickolaj.pocketnix.gui

import android.content.ComponentName
import android.content.Context
import android.content.pm.PackageManager
import android.content.pm.ShortcutInfo
import android.content.pm.ShortcutManager
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.Canvas
import android.graphics.Color
import android.graphics.Paint
import android.graphics.RectF
import android.graphics.drawable.Icon
import android.os.Handler
import android.os.HandlerThread
import android.util.Log
import dev.nickolaj.pocketnix.MainActivity
import dev.nickolaj.pocketnix.R
import kotlin.math.roundToInt

/**
 * Launcher shortcuts for the guest's apps: pins, and the recent apps on the
 * long-press menus of pocketnix's icon and the apps launcher's. Shortcut
 * calls run on their own thread: they are binder calls, and some throw when
 * Android rate-limits an app in the background.
 */
class AppShortcuts(private val context: Context, private val catalog: AppCatalog, private val iconSize: () -> Int) {
    private val handler = Handler(HandlerThread("gui-shortcuts").apply { start() }.looper)
    private val sm: ShortcutManager get() = context.getSystemService(ShortcutManager::class.java)
    private val syncNow = Runnable { guard("shortcuts: sync") { sync(catalog.apps.associateBy { it.id }) } }

    /** What [pin] did. */
    enum class Pin { ASKED, ALREADY, UNSUPPORTED }

    /** Brings pins and the recent apps in line with the catalog, soon: icons arrive in bursts. */
    fun update() {
        handler.removeCallbacks(syncNow)
        handler.postDelayed(syncNow, SYNC_DELAY_MS)
    }

    /**
     * Asks the launcher to pin [app]; only works while pocketnix is in the
     * foreground. A second request would add a second icon, so an app
     * already pinned (from any of its shortcuts) isn't asked again.
     */
    fun pin(app: AppEntry): Pin = guard("shortcuts: pin ${app.id}", Pin.UNSUPPORTED) {
        val sm = sm
        if (!sm.isRequestPinShortcutSupported) return@guard Pin.UNSUPPORTED
        if (sm.pinnedShortcuts.any { appOf(it.id) == app.id }) return@guard Pin.ALREADY
        sm.requestPinShortcut(info(app, PIN + app.id, null, 0), null)
        Pin.ASKED
    }

    /** Whether the apps launcher's icon is in the app drawer. */
    val launcherEnabled: Boolean
        get() = context.packageManager.getComponentEnabledSetting(launcher) == PackageManager.COMPONENT_ENABLED_STATE_ENABLED

    /** Shows or hides the apps launcher's icon, as the guest turns GUI on or off. */
    fun setLauncherEnabled(on: Boolean) {
        handler.post { setLauncher(on) }
    }

    /** Disables [app]'s pins now: a launch found it gone, whatever the cached list says. */
    fun disable(app: String) {
        handler.post {
            guard("shortcuts: disable $app") {
                val ids = sm.pinnedShortcuts.filter { it.isEnabled && appOf(it.id) == app }
                for (s in ids) sm.disableShortcuts(listOf(s.id), appGone((s.shortLabel ?: app).toString()))
            }
        }
    }

    private fun setLauncher(on: Boolean) {
        guard("shortcuts: launcher $on") {
            if (launcherEnabled == on) return@guard
            Log.i(TAG, "apps launcher ${if (on) "enabled" else "disabled"}")
            val state = if (on) PackageManager.COMPONENT_ENABLED_STATE_ENABLED else PackageManager.COMPONENT_ENABLED_STATE_DISABLED
            // Without DONT_KILL_APP, Android kills the process, and the VM with it.
            context.packageManager.setComponentEnabledSetting(launcher, state, PackageManager.DONT_KILL_APP)
            update()
        }
    }

    private fun sync(apps: Map<String, AppEntry>) {
        val sm = sm
        val pinned = sm.pinnedShortcuts
        val gone = pinned.filter { it.isEnabled && appOf(it.id) !in apps }
        if (gone.isNotEmpty()) {
            Log.i(TAG, "disabling pins of apps the guest no longer has: ${gone.map { it.id }}")
            for (s in gone) sm.disableShortcuts(listOf(s.id), appGone((s.shortLabel ?: appOf(s.id)).toString()))
        }
        val present = pinned.filter { appOf(it.id) in apps }
        val back = present.filter { !it.isEnabled }.map { it.id }
        if (back.isNotEmpty()) sm.enableShortcuts(back)
        // Names and icons change with a switch.
        if (present.isNotEmpty()) sm.updateShortcuts(present.map { info(apps.getValue(appOf(it.id)), it.id, null, 0) })

        val recent = catalog.recent.filter { !it.noDisplay }.take(RECENT_SHOWN)
        val activities = listOfNotNull(
            RECENT_MAIN to ComponentName(context, MainActivity::class.java),
            (RECENT_APPS to launcher).takeIf { launcherEnabled }
        )
        sm.dynamicShortcuts = activities.flatMap { (prefix, activity) ->
            recent.mapIndexed { rank, app -> info(app, prefix + app.id, activity, rank) }
        }
    }

    private fun info(app: AppEntry, id: String, activity: ComponentName?, rank: Int): ShortcutInfo = ShortcutInfo.Builder(context, id)
        .setShortLabel(app.label)
        .setLongLabel(app.label)
        .setIcon(icon(app))
        .setIntent(GuiWindowActivity.appIntent(context, app.id))
        .setRank(rank)
        .apply { if (activity != null) setActivity(activity) }
        .build()

    /** The cached icon centred on an adaptive icon's background, or a generic one. */
    private fun icon(app: AppEntry): Icon {
        val size = iconSize()
        val png = catalog.icon(app.icon, size)?.let { BitmapFactory.decodeFile(it.path) }
            ?: return Icon.createWithResource(context, R.drawable.ic_guest_app)
        // An adaptive icon is 108dp with a 72dp safe zone, which launchers mask further.
        val side = (size * ADAPTIVE_SIDE / ICON_SIDE).roundToInt()
        val out = Bitmap.createBitmap(side, side, Bitmap.Config.ARGB_8888)
        val c = Canvas(out)
        c.drawColor(Color.WHITE)
        val inset = (side - size) / 2f
        c.drawBitmap(png, null, RectF(inset, inset, inset + size, inset + size), Paint(Paint.FILTER_BITMAP_FLAG))
        return Icon.createWithAdaptiveBitmap(out)
    }

    private val launcher: ComponentName get() = ComponentName(context.packageName, LAUNCHER_ALIAS)

    companion object {
        private const val TAG = "AppShortcuts"
        private const val PIN = "app:"
        private const val RECENT_MAIN = "recent-main:"
        private const val RECENT_APPS = "recent-apps:"

        // Launchers show four on a long-press.
        private const val RECENT_SHOWN = 4
        private const val SYNC_DELAY_MS = 500L

        // The icon's share of the adaptive icon: 54dp of 108dp keeps a square's corners inside a round mask.
        private const val ICON_SIDE = 54f
        private const val ADAPTIVE_SIDE = 108f

        /** The activity-alias in the manifest; aliases are named after the code's package, not the application id. */
        const val LAUNCHER_ALIAS = "dev.nickolaj.pocketnix.AppsLauncher"

        /** The app a shortcut id names, whichever kind it is. */
        internal fun appOf(shortcutId: String): String = shortcutId.substringAfter(':')
    }
}
