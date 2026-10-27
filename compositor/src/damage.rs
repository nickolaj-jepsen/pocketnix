// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// From wprs's compositor_utils.rs, on smithay's viewport types.

use smithay::utils::Buffer as BufferCoords;
use smithay::utils::Logical;
use smithay::utils::Point;
use smithay::utils::Rectangle;
use smithay::utils::Size;
use smithay::utils::Transform;

/// A surface's wp_viewport state.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Viewport {
    pub src: Option<Rectangle<f64, Logical>>,
    pub dst: Option<Size<i32, Logical>>,
}

/// Convert a damage rectangle from surface coordinates to buffer coordinates.
///
/// The viewport is part of this mapping, not just buffer_scale: chromium leaves
/// buffer_scale at 1 and scales entirely through the viewport.
pub fn surface_damage_to_buffer(
    rect: Rectangle<i32, Logical>,
    buffer_scale: i32,
    transform: Transform,
    viewport: Viewport,
    buffer_size: Option<(i32, i32)>,
) -> Rectangle<i32, BufferCoords> {
    // The whole buffer in surface-local coordinates: to_logical applies
    // buffer_scale and transposes the axes for a rotating transform.
    let buffer_logical_size: Option<Size<f64, Logical>> = buffer_size.map(|(width, height)| {
        Size::<i32, BufferCoords>::from((width, height))
            .to_f64()
            .to_logical(f64::from(buffer_scale.max(1)), transform)
    });

    // The part of it the viewport samples, before the destination scaling.
    let src_size: Size<f64, Logical> = match (viewport.src, buffer_logical_size) {
        (Some(src), _) => src.size,
        (None, Some(size)) => size,
        (None, None) => return rect.to_buffer(buffer_scale, transform.invert(), &rect.size),
    };

    let (scale_w, scale_h) = match viewport.dst {
        Some(dst) if dst.w > 0 && dst.h > 0 => {
            (src_size.w / f64::from(dst.w), src_size.h / f64::from(dst.h))
        }
        _ => (1.0, 1.0),
    };
    let (offset_x, offset_y) = viewport.src.map_or((0.0, 0.0), |src| (src.loc.x, src.loc.y));

    let rect = rect.to_f64();
    let surface_local_rect = Rectangle::new(
        Point::from((rect.loc.x * scale_w + offset_x, rect.loc.y * scale_h + offset_y)),
        Size::from((rect.size.w * scale_w, rect.size.h * scale_h)),
    );

    // The rect spans the whole surface-local space now that src.loc has been
    // added, so a mirroring transform measures against that, not the crop.
    let area_size = buffer_logical_size.unwrap_or(src_size);
    let area = Size::from((area_size.w.ceil() as i32, area_size.h.ceil() as i32));

    // Round outwards: too much damage costs a repaint, too little leaves stale
    // pixels. buffer_transform is what the client already applied to the
    // buffer, so mapping back to buffer coordinates undoes it.
    surface_local_rect
        .to_i32_up::<i32>()
        .to_buffer(buffer_scale, transform.invert(), &area)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn viewport(src: Option<(f64, f64, f64, f64)>, dst: Option<(i32, i32)>) -> Viewport {
        Viewport {
            src: src.map(|(x, y, w, h)| Rectangle::new((x, y).into(), (w, h).into())),
            dst: dst.map(Size::from),
        }
    }

    fn damage(x: i32, y: i32, w: i32, h: i32) -> Rectangle<i32, Logical> {
        Rectangle::new((x, y).into(), (w, h).into())
    }

    fn buffer_rect(x: i32, y: i32, w: i32, h: i32) -> Rectangle<i32, BufferCoords> {
        Rectangle::new((x, y).into(), (w, h).into())
    }

    #[test]
    fn identity() {
        assert_eq!(
            surface_damage_to_buffer(damage(10, 20, 30, 40), 1, Transform::Normal, Viewport::default(), Some((100, 100))),
            buffer_rect(10, 20, 30, 40),
        );
    }

    #[test]
    fn buffer_scale_without_viewport() {
        assert_eq!(
            surface_damage_to_buffer(damage(10, 20, 30, 40), 2, Transform::Normal, Viewport::default(), Some((200, 200))),
            buffer_rect(20, 40, 60, 80),
        );
    }

    /// Chromium leaves buffer_scale at 1 and scales through the viewport, so
    /// the destination ratio is all that relates surface damage to the buffer.
    #[test]
    fn viewport_destination_scales_damage() {
        let vp = viewport(None, Some((1733, 729)));
        let buffer = Some((3466, 1458));
        assert_eq!(
            surface_damage_to_buffer(damage(0, 0, 1733, 729), 1, Transform::Normal, vp, buffer),
            buffer_rect(0, 0, 3466, 1458),
        );
        assert_eq!(
            surface_damage_to_buffer(damage(16, 153, 1701, 403), 1, Transform::Normal, vp, buffer),
            buffer_rect(32, 306, 3402, 806),
        );
    }

    #[test]
    fn viewport_source_crops_and_offsets() {
        let vp = viewport(Some((10.0, 10.0, 100.0, 50.0)), Some((200, 100)));
        assert_eq!(
            surface_damage_to_buffer(damage(0, 0, 200, 100), 1, Transform::Normal, vp, Some((400, 200))),
            buffer_rect(10, 10, 100, 50),
        );
    }

    #[test]
    fn rounds_outward() {
        let vp = viewport(None, Some((100, 100)));
        assert_eq!(
            surface_damage_to_buffer(damage(1, 1, 1, 1), 1, Transform::Normal, vp, Some((150, 150))),
            buffer_rect(1, 1, 2, 2),
        );
    }

    #[test]
    fn every_transform_maps_to_the_displayed_region() {
        let vp = viewport(Some((20.0, 20.0, 560.0, 360.0)), Some((600, 400)));
        let buffer = Some((1200, 800));
        let cases = [
            (Transform::Normal, buffer_rect(20, 20, 560, 360)),
            (Transform::_90, buffer_rect(20, 220, 360, 560)),
            (Transform::_180, buffer_rect(620, 420, 560, 360)),
            (Transform::_270, buffer_rect(820, 20, 360, 560)),
            (Transform::Flipped, buffer_rect(620, 20, 560, 360)),
            (Transform::Flipped90, buffer_rect(20, 20, 360, 560)),
            (Transform::Flipped180, buffer_rect(20, 420, 560, 360)),
            (Transform::Flipped270, buffer_rect(820, 220, 360, 560)),
        ];
        for (transform, expected) in cases {
            assert_eq!(
                surface_damage_to_buffer(damage(0, 0, 600, 400), 1, transform, vp, buffer),
                expected,
                "{transform:?}"
            );
        }
    }
}
